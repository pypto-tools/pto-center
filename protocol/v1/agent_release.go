package protocol

import (
	"archive/zip"
	"bytes"
	"debug/buildinfo"
	"errors"
	"fmt"
	"io"
	"regexp"
	"slices"
)

const (
	AgentTool           = "pto-agent"
	MaxAgentBundle      = 32 << 20
	MaxAgentBinary      = 48 << 20
	AgentVersionHeader  = "X-PTO-Agent-Version"
	AgentPlatformHeader = "X-PTO-Agent-Platform"
	AgentManagedHeader  = "X-PTO-Agent-Managed"
	agentManifest       = `{"apiVersion":"pto-center/v1","id":"pto-agent","source":{"type":"git","repository":"pypto-tools/pto-center"},"adapter":"builtin/agent","capabilities":["probe","update","rollback"]}`
	// A pre-management Agent interpreting this as an ordinary adapter can only
	// report unknown. Only the native updater may activate the payload.
	agentFallback = "#!/bin/sh\necho '{\"status\":\"succeeded\",\"data\":{\"presence\":\"unknown\"}}'\n"
)

var AgentPlatform = regexp.MustCompile(`^linux/[a-z0-9]{2,16}$`)

func binaryPlatform(binary []byte) (string, error) {
	info, err := buildinfo.Read(bytes.NewReader(binary))
	if err != nil || info.Path != "github.com/pypto-tools/pto-center/cmd/pto-agent" {
		return "", errors.New("expected a Go pto-agent executable")
	}
	var osName, arch string
	for _, s := range info.Settings {
		switch s.Key {
		case "GOOS":
			osName = s.Value
		case "GOARCH":
			arch = s.Value
		}
	}
	platform := osName + "/" + arch
	if !AgentPlatform.MatchString(platform) {
		return "", fmt.Errorf("unsupported agent platform %q", platform)
	}
	return platform, nil
}

func NewAgentRelease(version string, binaries [][]byte) (Release, error) {
	r := Release{Manifest: []byte(agentManifest), Version: version, Adapter: []byte(agentFallback)}
	if !Version.MatchString(version) || len(binaries) == 0 {
		return r, errors.New("provide a version and at least one pto-agent executable")
	}
	images := make(map[string][]byte)
	var platforms []string
	for _, b := range binaries {
		if len(b) > MaxAgentBinary {
			return r, errors.New("agent executable exceeds size limit")
		}
		platform, err := binaryPlatform(b)
		if err != nil {
			return r, err
		}
		if _, found := images[platform]; found {
			return r, fmt.Errorf("duplicate executable for %s", platform)
		}
		platforms = append(platforms, platform)
		images[platform] = b
	}
	slices.Sort(platforms)
	var buf bytes.Buffer
	w := zip.NewWriter(&buf)
	for _, platform := range platforms {
		entry, err := w.Create(platform)
		if err != nil {
			return r, err
		}
		if _, err = entry.Write(images[platform]); err != nil {
			return r, err
		}
	}
	if err := w.Close(); err != nil {
		return r, err
	}
	r.Payload = buf.Bytes()
	return r, nil
}

func agentArchive(data []byte) (*zip.Reader, []string, error) {
	z, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return nil, nil, err
	}
	if len(z.File) == 0 || len(z.File) > 32 {
		return nil, nil, errors.New("invalid number of agent platforms")
	}
	platforms := make([]string, 0, len(z.File))
	for _, f := range z.File {
		if !AgentPlatform.MatchString(f.Name) || !f.Mode().IsRegular() || f.UncompressedSize64 == 0 ||
			f.UncompressedSize64 > MaxAgentBinary || slices.Contains(platforms, f.Name) {
			return nil, nil, errors.New("invalid agent executable entry")
		}
		platforms = append(platforms, f.Name)
	}
	slices.Sort(platforms)
	return z, platforms, nil
}

// AgentImage reads a single bounded archive entry, never a path from the archive.
func (r Release) AgentImage(platform string) ([]byte, error) {
	z, _, err := agentArchive(r.Payload)
	if err != nil {
		return nil, err
	}
	for _, f := range z.File {
		if f.Name != platform {
			continue
		}
		reader, err := f.Open()
		if err != nil {
			return nil, err
		}
		defer reader.Close()
		binary, err := io.ReadAll(io.LimitReader(reader, MaxAgentBinary+1))
		if err != nil {
			return nil, err
		}
		actual, err := binaryPlatform(binary)
		if err != nil || len(binary) > MaxAgentBinary || actual != platform {
			return nil, errors.New("agent executable does not match this platform")
		}
		return binary, nil
	}
	return nil, fmt.Errorf("release has no executable for %s", platform)
}

func AgentInventory(version, platform string, managed bool) Inventory {
	caps := []string{"probe"}
	if managed {
		caps = append(caps, "update", "rollback")
	}
	return Inventory{Presence: "present", Platform: platform, Installed: version, Running: version, Capabilities: caps}
}
