package config

import (
	"fmt"
	"strconv"
	"strings"
)

// ServiceFlag collects repeated --service name=port flags.
type ServiceFlag struct {
	Entries map[string]uint16
}

func (s *ServiceFlag) String() string {
	if s == nil || len(s.Entries) == 0 {
		return ""
	}
	pairs := make([]string, 0, len(s.Entries))
	for name, port := range s.Entries {
		pairs = append(pairs, fmt.Sprintf("%s=%d", name, port))
	}
	return strings.Join(pairs, ",")
}

func (s *ServiceFlag) Set(value string) error {
	if value == "" {
		return fmt.Errorf("service definition is empty")
	}
	parts := strings.SplitN(value, "=", 2)
	if len(parts) != 2 {
		return fmt.Errorf("service must be in name=port format")
	}
	name := strings.TrimSpace(parts[0])
	if name == "" {
		return fmt.Errorf("service name is empty")
	}
	portValue := strings.TrimSpace(parts[1])
	port, err := strconv.Atoi(portValue)
	if err != nil || port <= 0 || port > 65535 {
		return fmt.Errorf("invalid port: %s", portValue)
	}
	if s.Entries == nil {
		s.Entries = map[string]uint16{}
	}
	s.Entries[name] = uint16(port)
	return nil
}
