package model

// SyslogMetadata separates sender claims (header fields on LogRecord) from
// transport evidence. TLSIdentity is an exact verified SAN mapping or a leaf
// certificate SHA-256 fingerprint; it is never the advertised HOSTNAME.
// RawPayload retains sanitized non-UTF-8 octets that JSON/protobuf strings
// cannot represent. Ordinary UTF-8 payloads use LogRecord.Raw only.
type SyslogMetadata struct {
	Version        uint32 `json:"version,omitempty"`
	MessageID      string `json:"message_id,omitempty"`
	StructuredData string `json:"structured_data,omitempty"`
	RawTimestamp   string `json:"raw_timestamp,omitempty"`
	Format         string `json:"format"`
	ParseStatus    string `json:"parse_status"`
	PeerIP         string `json:"peer_ip,omitempty"`
	Transport      string `json:"transport,omitempty"`
	TLSIdentity    string `json:"tls_identity,omitempty"`
	Authenticated  bool   `json:"authenticated"`
	Managed        bool   `json:"managed"`
	RawPayload     []byte `json:"raw_payload,omitempty"`
}
