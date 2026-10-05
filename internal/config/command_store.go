package config

import (
	"errors"
	"time"

	"InfraPulseAgent/internal/commands"
)

type CommandStore struct {
	JournalMaxEntries  int           `yaml:"journal_max_entries"`
	JournalMaxBytes    int           `yaml:"journal_max_bytes"`
	JournalRetention   time.Duration `yaml:"journal_retention"`
	SnapshotMaxEntries int           `yaml:"snapshot_max_entries"`
	SnapshotMaxBytes   int           `yaml:"snapshot_max_bytes"`
	SnapshotRetention  time.Duration `yaml:"snapshot_retention"`
}

func (c Config) CommandStoreOptions() commands.StoreOptions {
	v := c.Commands.Store
	d := commands.DefaultStoreOptions()
	if v.JournalMaxEntries != 0 {
		d.JournalMaxEntries = v.JournalMaxEntries
	}
	if v.JournalMaxBytes != 0 {
		d.JournalMaxBytes = v.JournalMaxBytes
	}
	if v.JournalRetention != 0 {
		d.JournalRetention = v.JournalRetention
	}
	if v.SnapshotMaxEntries != 0 {
		d.SnapshotMaxEntries = v.SnapshotMaxEntries
	}
	if v.SnapshotMaxBytes != 0 {
		d.SnapshotMaxBytes = v.SnapshotMaxBytes
	}
	if v.SnapshotRetention != 0 {
		d.SnapshotRetention = v.SnapshotRetention
	}
	return d
}

func (c Config) validateCommandStore() error {
	if err := c.CommandStoreOptions().Validate(); err != nil {
		return errors.New("invalid commands.store limits")
	}
	return nil
}
