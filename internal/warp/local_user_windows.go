//go:build windows

package warp

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"unsafe"

	"golang.org/x/sys/windows"
)

const warpUserStorageFileName = "dev.warp.Warp-User"

func defaultLocalUserStoragePath() (string, error) {
	localAppData := strings.TrimSpace(os.Getenv("LOCALAPPDATA"))
	if localAppData == "" {
		return "", fmt.Errorf("LOCALAPPDATA is not set")
	}
	return filepath.Join(localAppData, "warp", "Warp", "data", warpUserStorageFileName), nil
}

func decryptLocalUserStorage(encrypted []byte) (string, error) {
	if len(encrypted) == 0 {
		return "", nil
	}

	in := windows.DataBlob{
		Size: uint32(len(encrypted)),
		Data: &encrypted[0],
	}
	var out windows.DataBlob
	if err := windows.CryptUnprotectData(&in, nil, nil, 0, nil, 0, &out); err != nil {
		return "", err
	}
	defer windows.LocalFree(windows.Handle(unsafe.Pointer(out.Data)))

	decrypted := unsafe.Slice(out.Data, out.Size)
	return string(decrypted), nil
}
