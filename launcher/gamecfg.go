//go:build windows

package main

import (
	"os"
	"path/filepath"
	"strings"
)

// The game reads GAME:\XenonGameSettings.cfg at startup (Scripts/Game/Configure.lua,
// readCfg(currentPlatform .. "GameSettings.cfg")). Format: "Container.Field value" per
// line, '#' and '//' start comments. We only write settings that differ from the
// shipped defaults in Game.cfg.

const gameCfgMarker = "// Written by Bad Company Launcher"

func gameCfgPath() string { return filepath.Join(settings.GameData, "XenonGameSettings.cfg") }

func writeGameSettingsCfg() error {
	var lines []string
	if !settings.AutoAim {
		lines = append(lines, "Game.AutoAimEnabled false")
	}
	if !settings.MotionBlur {
		lines = append(lines, "Client.IsBlurEnabled false")
	}
	if !settings.FilmGrain {
		lines = append(lines, "PostProcess.FilmGrainEnable false")
	}
	if settings.hdrCompat() {
		// The Xbox 360 "fast HDR" path stores scene colours divided by this scale
		// in the 7e3 format (max ~32). The emulator does not clamp that format like
		// the hardware, so very bright effects overflow. A larger scale keeps them
		// in range. (Turning fast HDR off entirely crashes the game.)
		lines = append(lines, "WorldRender.XenonFastHdrColorScale 16")
	}
	if settings.fullResFx() {
		lines = append(lines, "EmitterSystem.QuadHalfResEnable false")
	}
	// Shadow distance (WorldRenderSettings; engine default 100 m, 1 shadow map slice
	// of 1024x1024). Always one slice: with more slices fire/smoke particles that
	// sample the shadow map turn into black clouds, and 3 slices at long range blew
	// distant terrain up into spikes. 250 m and more still broke terrain in
	// sp_04 cutscenes, so the options stay close to the original 100 m.
	switch settings.ShadowDistance {
	case "far":
		lines = append(lines, "WorldRender.ShadowmapViewDistance 150")
	case "ultra":
		lines = append(lines, "WorldRender.ShadowmapViewDistance 200")
	}
	path := gameCfgPath()
	if len(lines) == 0 {
		// Remove our file when everything is default (never delete a user-made file).
		if b, err := os.ReadFile(path); err == nil && strings.HasPrefix(string(b), gameCfgMarker) {
			return os.Remove(path)
		}
		return nil
	}
	if b, err := os.ReadFile(path); err == nil && !strings.HasPrefix(string(b), gameCfgMarker) {
		_ = os.WriteFile(path+".user_backup", b, 0o644)
	}
	content := gameCfgMarker + " v" + launcherVersion + "\r\n" + strings.Join(lines, "\r\n") + "\r\n"
	return os.WriteFile(path, []byte(content), 0o644)
}
