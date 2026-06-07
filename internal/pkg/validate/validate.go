// Package validate provides input validation and security checks for the skill-market service.
package validate

import (
	"fmt"
	"net"
	"net/url"
)

// SkillNamePattern defines allowed characters for skill names.
const SkillNamePattern = `^[a-zA-Z0-9_-]+$`

// ErrInvalidURL indicates a URL failed validation.
var ErrInvalidURL = fmt.Errorf("invalid URL: must be a valid http(s) URL")

// ErrSSRFBlocked indicates a URL points to an internal/private address.
var ErrSSRFBlocked = fmt.Errorf("URL points to a private/internal address (SSRF protection)")

// privateIPBlocks contains CIDR ranges for private/internal addresses.
var privateIPBlocks = []string{
	"10.0.0.0/8",
	"172.16.0.0/12",
	"192.168.0.0/16",
	"127.0.0.0/8",
	"169.254.0.0/16",
	"0.0.0.0/8",
	"::1/128",
	"fc00::/7",
	"fe80::/10",
}

var privateNets []*net.IPNet

func init() {
	for _, cidr := range privateIPBlocks {
		_, ipNet, err := net.ParseCIDR(cidr)
		if err == nil {
			privateNets = append(privateNets, ipNet)
		}
	}
}

// SourceURL validates a market source URL, ensuring it is a valid HTTP(S) URL
// and does not point to private/internal addresses (SSRF protection).
func SourceURL(raw string) error {
	if raw == "" {
		return fmt.Errorf("URL is required")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidURL, err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("%w: scheme must be http or https, got %s", ErrInvalidURL, u.Scheme)
	}
	if u.Host == "" {
		return fmt.Errorf("%w: host is required", ErrInvalidURL)
	}
	// SSRF check: resolve host and check if it's private
	host := u.Hostname()
	if host == "" {
		return fmt.Errorf("%w: host is required", ErrInvalidURL)
	}
	// Check for IP literals
	if ip := net.ParseIP(host); ip != nil {
		if isPrivateIP(ip) {
			return ErrSSRFBlocked
		}
		return nil
	}
	// For hostnames, attempt DNS resolution
	ips, err := net.LookupIP(host)
	if err != nil {
		// If DNS fails, allow but log a warning (handled by caller)
		return nil
	}
	for _, ip := range ips {
		if isPrivateIP(ip) {
			return ErrSSRFBlocked
		}
	}
	return nil
}

func isPrivateIP(ip net.IP) bool {
	for _, ipNet := range privateNets {
		if ipNet.Contains(ip) {
			return true
		}
	}
	return false
}

// SkillName validates a skill name against allowed characters.
func SkillName(name string) error {
	if name == "" {
		return fmt.Errorf("skill name is required")
	}
	if len(name) > 100 {
		return fmt.Errorf("skill name too long: max 100 characters")
	}
	for _, ch := range name {
		if !((ch >= 'a' && ch <= 'z') || (ch >= 'A' && ch <= 'Z') || (ch >= '0' && ch <= '9') || ch == '_' || ch == '-') {
			return fmt.Errorf("skill name contains invalid character: %q", ch)
		}
	}
	return nil
}

// SearchQuery validates a search query string.
func SearchQuery(q string) error {
	if len(q) > 200 {
		return fmt.Errorf("search query too long: max 200 characters")
	}
	return nil
}

// SourceName validates a market source name.
func SourceName(name string) error {
	if name == "" {
		return fmt.Errorf("source name is required")
	}
	if len(name) > 100 {
		return fmt.Errorf("source name too long: max 100 characters")
	}
	return nil
}

// ZipExtractLimits defines safety limits for ZIP extraction.
type ZipExtractLimits struct {
	MaxTotalSize  int64 // maximum total uncompressed size in bytes
	MaxFileCount  int   // maximum number of files
	MaxFileSize   int64 // maximum size of a single file
	MaxPathLength int   // maximum path length
}

// DefaultZipLimits provides sensible defaults for ZIP extraction.
var DefaultZipLimits = ZipExtractLimits{
	MaxTotalSize:  500 << 20, // 500 MB
	MaxFileCount:  10000,
	MaxFileSize:   100 << 20, // 100 MB per file
	MaxPathLength: 4096,
}
