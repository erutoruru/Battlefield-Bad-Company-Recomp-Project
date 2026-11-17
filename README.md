# Battlefield: Bad Company — PC (ReXGlue recompilation)

A PC port of Battlefield: Bad Company (Xbox 360, 2008, Title ID `454107F9`), statically recompiled with the [ReXGlue SDK](https://github.com/rexglue/rexglue-sdk). It comes with a launcher, mouse & keyboard support and graphics fixes.

> **No game files are included.** You need your own copy of the game, extracted from your disc/ISO. No game code is included either: it is generated on your PC from your own `default.xex` during installation.

*Türkçe açıklama aşağıda.*

## Installation (players)

0. The game must be the USA version; other versions are not supported. 0.0.0.6
1. Download the latest ZIP from **Releases** and extract it to a folder (e.g. `C:\BadCompany`).
2. Put the files extracted from your game disc into the **`assets`** folder, so that `assets\default.xex` and `assets\dist\...` exist.
3. Run **`BadCompanyLauncher.exe`**.
4. The first time, press **"Install game (once)"**. The window that opens:
   - installs missing build tools with `winget` (Visual Studio Build Tools C++, LLVM, CMake, Ninja),
   - downloads the ReXGlue SDK,
   - generates the game code from your `default.xex` and compiles `bfbc.exe`.

   This takes **15-40 minutes** depending on your connection and CPU, and is only done once.
5. When it finishes, adjust your settings in the launcher and press **PLAY**.

Double-clicking `Install.cmd` does the same as the button.

### Requirements
- Windows 10/11 64-bit, DirectX 12 capable GPU
- [Microsoft Edge WebView2 Runtime](https://developer.microsoft.com/microsoft-edge/webview2/) (preinstalled on Windows 11)
- About 20 GB of free space for the build
- Supported game version: Xbox 360 **USA**, `default.xex` SHA-256
  `ae42f7cc30ab3217af04f1c3c6a5ddb7e640d1e7df73b32f2877905a7d4d2670`

## Features

**Launcher** (`launcher/`, Go + WebView2)
- Resolution, monitor, fullscreen, VSync and shadow distance settings
- FOV (55-120°) and weapon size. These are applied to the level files; the originals are backed up to `assets\bfbc_backup`.
- Key bindings and in-game button prompts (Xbox / PlayStation / keyboard & mouse)
- Mouse, vehicle weapon and scope sensitivity; tank turret traverse speed
- Achievements page and online services (see `docs/ONLINE_API.md`)
- English and Turkish interface

**In game** (`src/mouse_look.cpp`)
- Direct mouse look without jitter; scopes use the game's own look-speed multipliers
- Smoothed mouse input for vehicle weapons
- Weapon selection with keys 1-5: main weapon, grenade launcher, knife and the items of the LB cycle. Picked-up items such as the RPG are learned too.
- Modifier keys (e.g. Shift to sprint, Ctrl to crouch)

**Graphics fixes** (`launcher/gamecfg.go` → `XenonGameSettings.cfg`)
- Blue smoke: particles are drawn at full resolution
- White explosions: HDR color scale fix
- Black smoke and terrain spikes: shadows always use a single slice, and the distance is capped at 200 m. Longer distances overflow the game's Xbox render budgets.

## Known issues
- Loading another save from the menu can occasionally crash; restarting the game is safer.
- Shadow distances above 200 m make the terrain explode into spikes in some levels, so the launcher does not offer them.

## Developers

```
launcher/            Go launcher (builds offline thanks to vendor/)
src/                 C++ code added to the game (mouse, keys, ReXGlue hooks)
tools/install.ps1    One-click install / build script
bfbc_manifest.toml   ReXGlue codegen settings (function addresses)
CMakeLists.txt, CMakePresets.json
docs/ONLINE_API.md   Online server API
```

**Build the game:** `Install.cmd`, or `tools\install.ps1 -SdkDir C:\rexglue\win-amd64`. For later builds, from an x64 Native Tools prompt:
```
cmake --build --preset win-amd64-release
```
The `generated/` folder is produced by `rexglue codegen`. Do not edit it and do not commit it: it is translated game code.

**Build the launcher** (Go 1.22+):
```
cd launcher
set GOOS=windows& set GOARCH=amd64
go build -ldflags "-H windowsgui -s -w" -o ..\BadCompanyLauncher.exe .
```

Notes:
- The hook addresses in `mouse_look.cpp` (`sub_8227DE90` aiming, `sub_82181878` controller) only match the USA `default.xex` listed above.
- The launcher modifies the level `.fbrb` archives (`fovpatch.go`) and the button prompt textures (`prompts.go`). Originals are backed up to `assets\bfbc_backup`.

## Legal
This project is not affiliated with Electronic Arts, DICE or Microsoft. "Battlefield" and "Bad Company" are trademarks of Electronic Arts Inc. This repository contains no game code, artwork, audio or other game files. You must provide the game from a copy you legally own.

---

## Türkçe

Battlefield: Bad Company'nin (Xbox 360) ReXGlue SDK ile PC'ye taşınmış hali. **Oyun dosyası ve oyun kodu dahil değildir.**

**Kurulum:**
1. **Releases** sayfasından ZIP'i indirip bir klasöre çıkar.
2. Oyunun ISO'sundan çıkardığın dosyaları **`assets`** klasörüne koy (`assets\default.xex` ve `assets\dist\...`).
3. `BadCompanyLauncher.exe`'yi aç. İlk seferde **"Install game (once)"** butonuna bas. Derleme araçları kurulur, SDK indirilir, oyunun kodu senin `default.xex` dosyandan üretilir ve `bfbc.exe` derlenir. Bu işlem 15-40 dakika sürer ve bir kere yapılır.
4. Ayarlarını yapıp **PLAY**'e bas.

Launcher'ın dilini **General** sekmesinden Türkçe yapabilirsin. Özellikler, bilinen sorunlar ve geliştirici notları için yukarıdaki İngilizce bölümlere bak.
