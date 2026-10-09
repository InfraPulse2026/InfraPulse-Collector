package remote

import (
	"context"
	"errors"
	"math/rand/v2"
	"sort"
	"time"

	"InfraPulseAgent/internal/config"
	"InfraPulseAgent/internal/driver"
	"InfraPulseAgent/internal/model"
	"github.com/google/uuid"
)

func batchInterval(adapter driver.Adapter, polling config.Polling) time.Duration {
	var interval time.Duration
	include := func(candidate time.Duration) {
		if candidate > 0 && (interval == 0 || candidate < interval) {
			interval = candidate
		}
	}
	if _, ok := adapter.(driver.InventoryBatchReader); ok {
		include(polling.InventoryInterval)
	}
	if _, ok := adapter.(driver.HardwareReader); ok {
		include(polling.MediumInterval)
	}
	return interval
}

// pollBatch shares the scheduler, target gate, observation binding and sink.
// The configured timeout covers the whole target poll, including both readers.
func (r *Registry) pollBatch(ctx context.Context, e *entry, now time.Time) error {
	if now.Before(e.nextBatchAllowed) {
		r.mu.Lock()
		message := e.state.LastError
		r.mu.Unlock()
		if message != "" {
			// Preserve degraded diagnostics during cooldown; this does not
			// perform another request or count another connectivity failure.
			return errors.New(message)
		}
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, e.cfg.Polling.Timeout)
	defer cancel()
	var errs []error
	contacted, successful := false, false
	var contactErr error
	recordContact := func(err error) {
		contacted = true
		if err == nil {
			successful = true
		} else if contactErr == nil || errors.Is(err, driver.ErrConnectivity) || errors.Is(err, context.DeadlineExceeded) {
			contactErr = err
		}
	}
	if reader, ok := e.driver.(driver.InventoryBatchReader); ok && !now.Before(e.nextInventory) {
		e.nextInventory = now.Add(e.cfg.Polling.InventoryInterval)
		inventory, err := reader.InventoryBatch(ctx)
		if err == nil && inventory.Completeness == "failed" {
			err = driver.ErrUnavailable
		}
		if inventory.ObservedAt.IsZero() {
			inventory.ObservedAt = now.UTC()
		}
		if err != nil || inventory.Completeness == "" {
			inventory.Completeness = "partial"
		}
		if inventory.Completeness != "complete" && inventory.Completeness != "partial" && inventory.Completeness != "failed" {
			err = errors.Join(err, driver.ErrMalformed)
			inventory.Completeness = "partial"
		}
		if boundErr := boundInventory(inventory, e.maxDevices, e.maxBatchItems); boundErr != nil {
			err = errors.Join(err, boundErr)
			recordContact(err)
		} else if len(inventory.Devices) > 0 || err == nil {
			recordContact(err)
			for i := range inventory.Devices {
				d := &inventory.Devices[i]
				d.Location = e.cfg.Location
				d.ManagementAddress = e.cfg.Address
				d.VendorHint = e.cfg.VendorHint
				*d = model.ObserveDevice(*d, r.source(e), inventory.ObservedAt, time.Now().UTC())
			}
			if cacheErr := r.inventoryBatch(e, &inventory); cacheErr != nil {
				err = errors.Join(err, cacheErr)
			}
			err = errors.Join(err, r.observation(e, model.Envelope{Inventory: &inventory}))
		} else {
			recordContact(err)
		}
		if err != nil {
			errs = append(errs, err)
		}
	}
	if ctx.Err() == nil {
		if reader, ok := e.driver.(driver.HardwareReader); ok && !now.Before(e.nextMedium) {
			e.nextMedium = now.Add(e.cfg.Polling.MediumInterval)
			samples, err := reader.Hardware(ctx)
			if boundErr := boundHardware(samples, e.maxDevices, e.maxBatchItems); boundErr != nil {
				err = errors.Join(err, boundErr)
				recordContact(err)
			} else {
				recordContact(err)
				for _, sample := range samples {
					if sample.ObservedAt.IsZero() {
						sample.ObservedAt = now.UTC()
					}
					if sample.CollectedAt.IsZero() {
						sample.CollectedAt = time.Now().UTC()
					}
					err = errors.Join(err, r.observation(e, model.Envelope{Telemetry: &sample}))
				}
			}
			if err != nil {
				errs = append(errs, err)
			}
		}
	}
	if ctx.Err() != nil {
		errs = append(errs, ctx.Err())
		contactErr = ctx.Err()
	}
	if contacted && !errors.Is(contactErr, context.Canceled) {
		// A valid prefix never hides an authentication/network failure on the
		// same endpoint. Other optional resource failures retain partial data.
		if successful && contactErr == nil {
			r.batchConnectivity(e, nil, now)
			e.batchFailures, e.nextBatchAllowed = 0, time.Time{}
		} else if contactErr != nil {
			r.batchConnectivity(e, contactErr, now)
			if errors.Is(contactErr, driver.ErrConnectivity) || errors.Is(contactErr, context.DeadlineExceeded) || errors.Is(contactErr, driver.ErrUnavailable) {
				e.deferBatch(now)
			}
		}
	}
	return r.collectionResult(e, errors.Join(errs...), now)
}

func (e *entry) deferBatch(now time.Time) {
	if e.batchFailures < 16 {
		e.batchFailures++
	}
	delay := min(batchInterval(e.driver, e.cfg.Polling), 5*time.Minute)
	for i := uint8(1); i < e.batchFailures && delay < 5*time.Minute; i++ {
		delay = min(2*delay, 5*time.Minute)
	}
	// Independent bounded jitter spreads retries across BMCs without extra
	// tickers, goroutines or changes to the shared scheduler's worker budget.
	if delay > 0 {
		delay = min(delay+time.Duration(rand.Int64N(int64(delay/10)+1)), 5*time.Minute)
	}
	e.nextBatchAllowed = now.Add(delay)
}

func boundInventory(inventory model.InventoryUpdate, maxDevices, maxItems int) error {
	if len(inventory.Devices) > maxDevices {
		return driver.ErrLimit
	}
	seen := make(map[string]bool, len(inventory.Devices))
	items := len(inventory.Devices)
	for _, device := range inventory.Devices {
		if device.ID == "" || seen[device.ID] {
			return driver.ErrMalformed
		}
		seen[device.ID] = true
		items += len(device.Components)
		if items > maxItems {
			return driver.ErrLimit
		}
	}
	return nil
}

func boundHardware(samples []model.TelemetrySample, maxDevices, maxItems int) error {
	if len(samples) > maxDevices {
		return driver.ErrLimit
	}
	seen := make(map[string]bool, len(samples))
	items := len(samples)
	for _, sample := range samples {
		if sample.DeviceID == "" || seen[sample.DeviceID] {
			return driver.ErrMalformed
		}
		seen[sample.DeviceID] = true
		items += len(sample.Metrics) + len(sample.HardwareHealth)
		if items > maxItems {
			return driver.ErrLimit
		}
	}
	return nil
}

func (r *Registry) inventoryBatch(e *entry, inventory *model.InventoryUpdate) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	cache := make(map[string]model.Device, len(e.state.Devices)+len(inventory.Devices))
	if inventory.Completeness != "complete" {
		for _, old := range e.state.Devices {
			cache[old.ID] = model.CloneDevice(old)
		}
	}
	for _, device := range inventory.Devices {
		if old, ok := cache[device.ID]; ok {
			device = mergePartialDevice(old, device)
		}
		cache[device.ID] = model.CloneDevice(device)
	}
	devices := make([]model.Device, 0, len(cache))
	for _, device := range cache {
		devices = append(devices, device)
	}
	if err := boundInventory(model.InventoryUpdate{Devices: devices}, e.maxDevices, e.maxBatchItems); err != nil {
		return err
	}
	for i := range inventory.Devices {
		inventory.Devices[i] = model.CloneDevice(cache[inventory.Devices[i].ID])
	}
	sort.Slice(devices, func(i, j int) bool { return devices[i].ID < devices[j].ID })
	e.state.Devices = devices
	return nil
}

func mergePartialDevice(old, fresh model.Device) model.Device {
	// Preserve inventory facts absent from a partial response. Resource identity
	// is the explicit key; facts are never copied between different resources.
	for _, field := range []struct {
		dest     *string
		previous string
	}{
		{&fresh.Name, old.Name}, {&fresh.Hostname, old.Hostname}, {&fresh.Vendor, old.Vendor},
		{&fresh.Model, old.Model}, {&fresh.SerialNumber, old.SerialNumber}, {&fresh.HardwareRevision, old.HardwareRevision},
		{&fresh.SoftwareVersion, old.SoftwareVersion}, {&fresh.FirmwareVersion, old.FirmwareVersion},
	} {
		if *field.dest == "" {
			*field.dest = field.previous
		}
	}
	components := make(map[string]model.InventoryComponent, len(old.Components)+len(fresh.Components))
	for _, component := range old.Components {
		components[component.ID] = component
	}
	for _, component := range fresh.Components {
		if previous, ok := components[component.ID]; ok {
			component = mergePartialComponent(previous, component)
		}
		components[component.ID] = component
	}
	fresh.Components = make([]model.InventoryComponent, 0, len(components))
	for _, component := range components {
		fresh.Components = append(fresh.Components, component)
	}
	sort.Slice(fresh.Components, func(i, j int) bool { return fresh.Components[i].ID < fresh.Components[j].ID })
	fresh.IdentityEvidence = mergeIdentityEvidence(old.IdentityEvidence, fresh.IdentityEvidence)
	fresh.Hardware = mergePartialHardware(old.Hardware, fresh.Hardware)
	return model.CloneDevice(fresh)
}

func mergePartialComponent(old, fresh model.InventoryComponent) model.InventoryComponent {
	for _, field := range []struct {
		dest     *string
		previous string
	}{
		{&fresh.ParentID, old.ParentID}, {&fresh.Class, old.Class}, {&fresh.Name, old.Name},
		{&fresh.Description, old.Description}, {&fresh.Manufacturer, old.Manufacturer}, {&fresh.Model, old.Model},
		{&fresh.SerialNumber, old.SerialNumber}, {&fresh.HardwareRevision, old.HardwareRevision},
		{&fresh.FirmwareVersion, old.FirmwareVersion}, {&fresh.SoftwareVersion, old.SoftwareVersion},
	} {
		if *field.dest == "" {
			*field.dest = field.previous
		}
	}
	fresh.Hardware = mergePartialHardware(old.Hardware, fresh.Hardware)
	return fresh
}

func mergePartialHardware(old, fresh *model.HardwareInventory) *model.HardwareInventory {
	if old == nil {
		return model.CloneHardware(fresh)
	}
	if fresh == nil {
		return model.CloneHardware(old)
	}
	h := model.CloneHardware(fresh)
	for _, field := range []struct {
		dest     *string
		previous string
	}{
		{&h.ResourceType, old.ResourceType}, {&h.HardwareUUID, old.HardwareUUID}, {&h.SKU, old.SKU},
		{&h.PartNumber, old.PartNumber}, {&h.BIOSVersion, old.BIOSVersion}, {&h.PowerState, old.PowerState},
		{&h.ChassisType, old.ChassisType}, {&h.ManagerType, old.ManagerType}, {&h.AssetTag, old.AssetTag},
		{&h.PhysicalSecurity, old.PhysicalSecurity}, {&h.DeviceLocation, old.DeviceLocation},
		{&h.MemoryType, old.MemoryType}, {&h.ErrorCorrection, old.ErrorCorrection}, {&h.Protocol, old.Protocol}, {&h.MediaType, old.MediaType},
	} {
		if *field.dest == "" {
			*field.dest = field.previous
		}
	}
	previous := model.CloneHardware(old)
	if h.CapacityBytes == nil {
		h.CapacityBytes = previous.CapacityBytes
	}
	if h.Cores == nil {
		h.Cores = previous.Cores
	}
	if h.Threads == nil {
		h.Threads = previous.Threads
	}
	if len(h.NetworkAddresses) == 0 {
		h.NetworkAddresses = previous.NetworkAddresses
	}
	if len(h.Relations) == 0 {
		h.Relations = previous.Relations
	}
	return h
}

func mergeIdentityEvidence(old, fresh []model.IdentityEvidence) []model.IdentityEvidence {
	seen := make(map[string]bool, len(old)+len(fresh))
	result := make([]model.IdentityEvidence, 0, len(old)+len(fresh))
	for _, group := range [][]model.IdentityEvidence{fresh, old} {
		for _, evidence := range group {
			// Keep current evidence per kind/subject rather than accumulating an
			// unbounded history if a resource's manufacturer/serial changes.
			key := string(evidence.Type) + "\x00" + evidence.SubjectID
			if !seen[key] {
				result = append(result, evidence)
				seen[key] = true
			}
		}
	}
	return result
}

func (r *Registry) batchConnectivity(e *entry, err error, at time.Time) {
	r.mu.Lock()
	wasOnline := e.state.Device.Online
	transition := ""
	if err == nil {
		e.state.ConsecutiveConnectivityFailures = 0
		e.state.Device.LastSeen, e.state.Device.Online = at.UTC(), true
		e.state.ConnectivityState = "online"
		if !wasOnline {
			transition = "target.online"
		}
	} else if errors.Is(err, driver.ErrConnectivity) || errors.Is(err, context.DeadlineExceeded) {
		e.state.ConsecutiveConnectivityFailures++
		if e.state.ConsecutiveConnectivityFailures >= 3 && e.state.ConnectivityState != "offline" {
			e.state.Device.Online = false
			e.state.ConnectivityState = "offline"
			transition = "target.offline"
		}
	} else if !errors.Is(err, context.Canceled) {
		e.state.ConsecutiveConnectivityFailures = 0
	}
	r.mu.Unlock()
	if transition != "" {
		// This describes the configured source endpoint, not a new equipment
		// entity; DeviceID is intentionally absent and SourceRef has TargetID.
		_ = r.observation(e, model.Envelope{Events: []model.Event{{ID: uuid.NewString(), Type: transition, Timestamp: at.UTC(), Severity: "info", Message: transition, Source: e.cfg.Driver}}})
	}
}
