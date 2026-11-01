//go:build windows

package main

import (
	"encoding/base64"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

var (
	user32  = windows.NewLazySystemDLL("user32.dll")
	shell32 = windows.NewLazySystemDLL("shell32.dll")
	ole32   = windows.NewLazySystemDLL("ole32.dll")

	procMessageBoxW          = user32.NewProc("MessageBoxW")
	procGetDpiForSystem      = user32.NewProc("GetDpiForSystem")
	procEnumDisplayMonitors  = user32.NewProc("EnumDisplayMonitors")
	procGetMonitorInfoW      = user32.NewProc("GetMonitorInfoW")
	procSHBrowseForFolderW   = shell32.NewProc("SHBrowseForFolderW")
	procSHGetPathFromIDListW = shell32.NewProc("SHGetPathFromIDListW")
	procCoTaskMemFree        = ole32.NewProc("CoTaskMemFree")
)

func showError(msg string) {
	t, _ := syscall.UTF16PtrFromString("Bad Company Launcher")
	m, _ := syscall.UTF16PtrFromString(msg)
	procMessageBoxW.Call(0, uintptr(unsafe.Pointer(m)), uintptr(unsafe.Pointer(t)), 0x10)
}

// dpiScale returns the system DPI scale factor (1.0 = 100%).
func dpiScale() float64 {
	if err := procGetDpiForSystem.Find(); err != nil {
		return 1.0
	}
	dpi, _, _ := procGetDpiForSystem.Call()
	if dpi == 0 {
		return 1.0
	}
	return float64(dpi) / 96.0
}

// --- Folder picker -----------------------------------------------------------

type browseInfo struct {
	Owner        uintptr
	Root         uintptr
	DisplayName  *uint16
	Title        *uint16
	Flags        uint32
	CallbackFunc uintptr
	LParam       uintptr
	Image        int32
}

func browseFolder(title string) (string, error) {
	display := make([]uint16, windows.MAX_PATH)
	t, _ := syscall.UTF16PtrFromString(title)
	var owner uintptr
	if view != nil {
		owner = uintptr(view.Window())
	}
	bi := browseInfo{
		Owner:       owner,
		DisplayName: &display[0],
		Title:       t,
		Flags:       0x0001 | 0x0040, // BIF_RETURNONLYFSDIRS | BIF_NEWDIALOGSTYLE
	}
	pidl, _, _ := procSHBrowseForFolderW.Call(uintptr(unsafe.Pointer(&bi)))
	if pidl == 0 {
		return "", nil // cancelled
	}
	defer procCoTaskMemFree.Call(pidl)
	path := make([]uint16, windows.MAX_PATH)
	ok, _, _ := procSHGetPathFromIDListW.Call(pidl, uintptr(unsafe.Pointer(&path[0])))
	if ok == 0 {
		return "", errors.New("path_error")
	}
	return windows.UTF16ToString(path), nil
}

// --- Monitors / GPUs ----------------------------------------------------------

type monitorInfo struct {
	CbSize  uint32
	Monitor windows.Rect
	Work    windows.Rect
	Flags   uint32
}

func listMonitors() []string {
	var out []string
	cb := syscall.NewCallback(func(hMon, hdc, rect, lparam uintptr) uintptr {
		mi := monitorInfo{CbSize: uint32(unsafe.Sizeof(monitorInfo{}))}
		procGetMonitorInfoW.Call(hMon, uintptr(unsafe.Pointer(&mi)))
		w := mi.Monitor.Right - mi.Monitor.Left
		h := mi.Monitor.Bottom - mi.Monitor.Top
		primary := mi.Flags&1 != 0
		label := fmt.Sprintf("%d×%d", w, h)
		if primary {
			label += " *"
		}
		out = append(out, label)
		return 1
	})
	procEnumDisplayMonitors.Call(0, 0, cb, 0)
	return out
}

type GPU struct {
	Name string `json:"name"`
	Pref int    `json:"pref"` // Windows GPU preference: 1 = power saving (integrated), 2 = high performance
}

func gpuPreferenceFor(name string) int {
	n := strings.ToLower(name)
	switch {
	case strings.Contains(n, "nvidia"), strings.Contains(n, "radeon rx"), strings.Contains(n, "radeon pro"),
		strings.Contains(n, "arc("), strings.Contains(n, "arc a"), strings.Contains(n, "arc b"):
		return 2
	case strings.Contains(n, "intel"), strings.Contains(n, "radeon"):
		return 1 // integrated (e.g. "AMD Radeon(TM) Graphics", "Intel(R) UHD Graphics")
	}
	return 2
}

func listGPUInfo() []GPU {
	var out []GPU
	for _, n := range listGPUs() {
		out = append(out, GPU{Name: n, Pref: gpuPreferenceFor(n)})
	}
	return out
}

// applyGpuPreference sets the per-app Windows graphics preference for the game exe
// (same as Settings > System > Display > Graphics). 0 removes the override.
func applyGpuPreference(exe string, pref int) error {
	k, _, err := registry.CreateKey(registry.CURRENT_USER,
		`Software\Microsoft\DirectX\UserGpuPreferences`, registry.SET_VALUE|registry.QUERY_VALUE)
	if err != nil {
		return err
	}
	defer k.Close()
	if pref != 1 && pref != 2 {
		err := k.DeleteValue(exe)
		if err == registry.ErrNotExist {
			return nil
		}
		return err
	}
	return k.SetStringValue(exe, fmt.Sprintf("GpuPreference=%d;", pref))
}

var gpuCache []string

func listGPUs() []string {
	if gpuCache != nil {
		return gpuCache
	}
	cmd := exec.Command("powershell", "-NoProfile", "-Command",
		"(Get-CimInstance Win32_VideoController).Name")
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: 0x08000000}
	b, err := cmd.Output()
	gpuCache = []string{}
	if err == nil {
		for _, l := range strings.Split(string(b), "\n") {
			if l = strings.TrimSpace(l); l != "" {
				gpuCache = append(gpuCache, l)
			}
		}
	}
	return gpuCache
}

// --- Token protection (Windows DPAPI, current user only) ---------------------

func encryptToken(tok string) (string, error) {
	if tok == "" {
		return "", nil
	}
	in := windows.DataBlob{Size: uint32(len(tok)), Data: &[]byte(tok)[0]}
	var out windows.DataBlob
	if err := windows.CryptProtectData(&in, nil, nil, 0, nil, 0x1 /*UI_FORBIDDEN*/, &out); err != nil {
		return "", err
	}
	defer windows.LocalFree(windows.Handle(unsafe.Pointer(out.Data)))
	b := unsafe.Slice(out.Data, out.Size)
	return base64.StdEncoding.EncodeToString(b), nil
}

func decryptToken(enc string) (string, error) {
	if enc == "" {
		return "", nil
	}
	raw, err := base64.StdEncoding.DecodeString(enc)
	if err != nil || len(raw) == 0 {
		return "", errors.New("bad_token")
	}
	in := windows.DataBlob{Size: uint32(len(raw)), Data: &raw[0]}
	var out windows.DataBlob
	if err := windows.CryptUnprotectData(&in, nil, nil, 0, nil, 0x1, &out); err != nil {
		return "", err
	}
	defer windows.LocalFree(windows.Handle(unsafe.Pointer(out.Data)))
	return string(unsafe.Slice(out.Data, out.Size)), nil
}
