package grpc

import (
	"errors"
	"net/netip"
	"slices"
	"unicode/utf8"

	pb "InfraPulseAgent/api/gen/infrapulse/v1"
	"InfraPulseAgent/internal/model"
)

func logStringsBound(limit int, fields ...string) error {
	for _, value := range fields {
		if len(value) > limit || !utf8.ValidString(value) {
			return errors.New("invalid or oversized log text")
		}
	}
	return nil
}

func validateSyslogMetadata(s *model.SyslogMetadata) error {
	if s == nil {
		return nil
	}
	if err := logStringsBound(65536, s.StructuredData); err != nil {
		return err
	}
	if err := logStringsBound(1024, s.MessageID, s.RawTimestamp, s.TLSIdentity); err != nil {
		return err
	}
	if err := logStringsBound(128, s.PeerIP, s.Format, s.ParseStatus, s.Transport); err != nil {
		return err
	}
	if s.Format != "rfc5424" && s.Format != "rfc3164" && s.Format != "unknown" ||
		s.ParseStatus != "parsed" && s.ParseStatus != "fallback" || s.Version > 999 {
		return errors.New("invalid syslog parsing metadata")
	}
	if s.Transport != "" && s.Transport != "tls" && s.Transport != "tcp" {
		return errors.New("invalid syslog transport")
	}
	if s.PeerIP != "" {
		peer, err := netip.ParseAddr(s.PeerIP)
		if err != nil || peer.Zone() != "" {
			return errors.New("invalid syslog peer IP")
		}
	}
	if s.Authenticated && (s.Transport != "tls" || s.TLSIdentity == "") ||
		s.TLSIdentity != "" && (!s.Authenticated || s.Transport != "tls") {
		return errors.New("invalid syslog authentication evidence")
	}
	if len(s.RawPayload) > 65536 || len(s.RawPayload) > 0 && utf8.Valid(s.RawPayload) {
		return errors.New("invalid syslog raw octets")
	}
	return nil
}

func syslogToProto(s *model.SyslogMetadata) *pb.SyslogMetadata {
	if s == nil {
		return nil
	}
	return &pb.SyslogMetadata{Version: s.Version, MessageId: s.MessageID, StructuredData: s.StructuredData,
		RawTimestamp: s.RawTimestamp, Format: s.Format, ParseStatus: s.ParseStatus, PeerIp: s.PeerIP,
		Transport: s.Transport, TlsIdentity: s.TLSIdentity, Authenticated: s.Authenticated,
		Managed: s.Managed, RawPayload: slices.Clone(s.RawPayload)}
}

func syslogFromProto(s *pb.SyslogMetadata) (*model.SyslogMetadata, error) {
	if s == nil {
		return nil, nil
	}
	metadata := &model.SyslogMetadata{Version: s.Version, MessageID: s.MessageId, StructuredData: s.StructuredData,
		RawTimestamp: s.RawTimestamp, Format: s.Format, ParseStatus: s.ParseStatus, PeerIP: s.PeerIp,
		Transport: s.Transport, TLSIdentity: s.TlsIdentity, Authenticated: s.Authenticated,
		Managed: s.Managed, RawPayload: s.RawPayload}
	if err := validateSyslogMetadata(metadata); err != nil {
		return nil, err
	}
	metadata.RawPayload = slices.Clone(s.RawPayload)
	return metadata, nil
}
