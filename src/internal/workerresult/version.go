package workerresult

import (
	"errors"
	"fmt"
	"regexp"
	"strconv"
)

// ProtocolVersion is the worker result protocol version (WBS 13.3). Major
// versions are incompatible; minors add backward-compatible fields.
type ProtocolVersion struct {
	Major int
	Minor int
}

// Supported protocol: 1.0.
var (
	ProtocolV1_0 = ProtocolVersion{Major: 1, Minor: 0}
	// SupportedProtocol is the newest version this parser fully knows.
	SupportedProtocol = ProtocolV1_0
)

// Version errors.
var (
	ErrVersionMalformed   = errors.New("workerresult: malformed protocol version")
	ErrVersionUnsupported = errors.New("workerresult: unsupported protocol major version")
)

var versionRE = regexp.MustCompile(`^(0|[1-9][0-9]{0,3})\.(0|[1-9][0-9]{0,3})$`)

// ParseProtocolVersion parses "MAJOR.MINOR".
func ParseProtocolVersion(s string) (ProtocolVersion, error) {
	m := versionRE.FindStringSubmatch(s)
	if m == nil {
		return ProtocolVersion{}, fmt.Errorf("%w: %q", ErrVersionMalformed, s)
	}
	maj, _ := strconv.Atoi(m[1])
	min, _ := strconv.Atoi(m[2])
	return ProtocolVersion{Major: maj, Minor: min}, nil
}

func (v ProtocolVersion) String() string { return fmt.Sprintf("%d.%d", v.Major, v.Minor) }

// Compatible applies the compatibility policy: the supported major is
// accepted at any minor.
func (v ProtocolVersion) Compatible() error {
	if v.Major != SupportedProtocol.Major {
		return fmt.Errorf("%w: %s", ErrVersionUnsupported, v)
	}
	return nil
}

// ForwardCompatible reports a newer minor than this parser knows (its
// unknown fields are ignored, never applied).
func (v ProtocolVersion) ForwardCompatible() bool {
	return v.Major == SupportedProtocol.Major && v.Minor > SupportedProtocol.Minor
}
