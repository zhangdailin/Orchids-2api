package grok

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"orchids-api/internal/config"
)

const sharedMediaMarkerName = ".orchids-cluster"

// ConfigureMediaStorage selects the local media root and validates an
// operator-declared shared mount before any media worker starts.
func ConfigureMediaStorage(cfg *config.Config) error {
	if cfg == nil {
		return errors.New("config is required")
	}
	directory := strings.TrimSpace(cfg.MediaDir)
	if directory == "" {
		directory = filepath.Join("data", "tmp")
	}
	directory = filepath.Clean(directory)
	if cfg.DeploymentReplicas < 1 {
		return errors.New("deployment_replicas must be at least 1")
	}
	if err := os.MkdirAll(directory, 0o755); err != nil {
		return fmt.Errorf("create media directory: %w", err)
	}
	for _, kind := range []string{"image", "video"} {
		if err := os.MkdirAll(filepath.Join(directory, kind), 0o755); err != nil {
			return fmt.Errorf("create %s media directory: %w", kind, err)
		}
	}
	if cfg.DeploymentReplicas > 1 {
		if !strings.EqualFold(strings.TrimSpace(cfg.StoreMode), "redis") || strings.TrimSpace(cfg.RedisAddr) == "" {
			return errors.New("multi-instance deployment requires Redis storage")
		}
		if strings.TrimSpace(cfg.DeploymentInstance) == "" {
			return errors.New("multi-instance deployment requires deployment_instance_id")
		}
		if strings.TrimSpace(cfg.DeploymentCluster) == "" {
			return errors.New("multi-instance deployment requires deployment_cluster_id")
		}
		if !cfg.SharedMedia {
			return errors.New("multi-instance deployment requires shared_media=true and a shared media_dir mount")
		}
		if err := preflightSharedMedia(directory, cfg.DeploymentCluster, cfg.DeploymentInstance); err != nil {
			return err
		}
	}
	cacheBaseDir = directory
	return nil
}

// CacheBaseDir returns the media root the package is writing to, after
// ConfigureMediaStorage has resolved the default and cleaned the configured value.
//
// A caller outside this package needs the resolved path, not the configured one:
// the sweeper walks the same tree the media inputs are written to, and the default
// ("data/tmp" when media_dir is unset) is applied here rather than in the config.
// Before ConfigureMediaStorage runs it returns the package default, which is the
// same value the package would have used.
func CacheBaseDir() string {
	return cacheBaseDir
}

func preflightSharedMedia(directory, clusterID, instanceID string) error {
	markerPath := filepath.Join(directory, sharedMediaMarkerName)
	want := strings.TrimSpace(clusterID) + "\n"
	current, err := os.ReadFile(markerPath)
	if errors.Is(err, os.ErrNotExist) {
		file, createErr := os.OpenFile(markerPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		switch {
		case createErr == nil:
			if _, writeErr := file.WriteString(want); writeErr != nil {
				_ = file.Close()
				return fmt.Errorf("write shared media cluster marker: %w", writeErr)
			}
			if closeErr := file.Close(); closeErr != nil {
				return fmt.Errorf("close shared media cluster marker: %w", closeErr)
			}
			current = []byte(want)
		case errors.Is(createErr, os.ErrExist):
			current, err = os.ReadFile(markerPath)
			if err != nil {
				return fmt.Errorf("read shared media cluster marker: %w", err)
			}
		default:
			return fmt.Errorf("create shared media cluster marker: %w", createErr)
		}
	} else if err != nil {
		return fmt.Errorf("read shared media cluster marker: %w", err)
	}
	if string(current) != want {
		return fmt.Errorf("shared media cluster marker mismatch: configured cluster %q does not own %s", strings.TrimSpace(clusterID), markerPath)
	}

	probe, err := os.CreateTemp(directory, ".orchids-preflight-")
	if err != nil {
		return fmt.Errorf("create shared media preflight file: %w", err)
	}
	probePath := probe.Name()
	defer os.Remove(probePath)
	payload := []byte(strings.TrimSpace(instanceID))
	if _, err := probe.Write(payload); err != nil {
		_ = probe.Close()
		return fmt.Errorf("write shared media preflight file: %w", err)
	}
	if err := probe.Close(); err != nil {
		return fmt.Errorf("close shared media preflight file: %w", err)
	}
	readBack, err := os.ReadFile(probePath)
	if err != nil {
		return fmt.Errorf("read shared media preflight file: %w", err)
	}
	if string(readBack) != string(payload) {
		return errors.New("shared media preflight read-back mismatch")
	}
	return nil
}
