package storageconfig

import (
	"context"
	"fmt"
	"net"
	"regexp"
	"strings"
	"time"
)

// validateSMBReachable only proves the SMB port accepts connections; the
// gateway has no SMB client, so credentials are checked when the CSI driver
// mounts the share.
func validateSMBReachable(ctx context.Context, host, port string) error {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	var dialer net.Dialer
	conn, err := dialer.DialContext(ctx, "tcp", net.JoinHostPort(host, port))
	if err != nil {
		return fmt.Errorf("connect smb server: %w", err)
	}
	return conn.Close()
}

// NormalizeType maps an omitted type to s3 for backward compatibility.
func NormalizeType(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	if value == "" {
		return TypeS3
	}
	return value
}

var smbHostPattern = regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9.-]*[A-Za-z0-9])?$`)

// cleanSMBPath trims slashes and rejects segments that could escape the share
// or break the rclone path.
func cleanSMBPath(field, value string, allowNested bool) (string, error) {
	value = strings.Trim(strings.TrimSpace(strings.ReplaceAll(value, "\\", "/")), "/")
	if value == "" {
		return "", nil
	}
	segments := strings.Split(value, "/")
	if !allowNested && len(segments) > 1 {
		return "", fmt.Errorf("%s must be a single share name", field)
	}
	for _, segment := range segments {
		if segment == "" || segment == "." || segment == ".." || strings.ContainsAny(segment, "\x00\r\n") {
			return "", fmt.Errorf("invalid %s %q", field, value)
		}
	}
	return value, nil
}

func normalizeSMBFields(req ClassRequest, stored *StoredClass) error {
	host := strings.TrimSpace(req.Host)
	host = strings.TrimPrefix(strings.TrimPrefix(host, "smb://"), "//")
	if !smbHostPattern.MatchString(host) {
		return fmt.Errorf("invalid smb host %q: use a hostname or IP without scheme, port or path", req.Host)
	}
	share, err := cleanSMBPath("share", req.Share, false)
	if err != nil {
		return err
	}
	subPath, err := cleanSMBPath("sub_path", req.SubPath, true)
	if err != nil {
		return err
	}
	for _, field := range []struct{ name, value string }{
		{"username", req.Username}, {"password", req.Password}, {"domain", req.Domain},
	} {
		if strings.ContainsAny(field.value, "\r\n") {
			return fmt.Errorf("%s must not contain line breaks", field.name)
		}
	}
	stored.Type = TypeSMB
	stored.Host, stored.Share, stored.SubPath = host, share, subPath
	stored.Username = strings.TrimSpace(req.Username)
	stored.Password = req.Password
	stored.Domain = strings.TrimSpace(req.Domain)
	return nil
}
