// Field of view and weapon (view model) size.
//
// Both live in the binary .dbx data inside every levels\*\level-00.fbrb:
//   - SoldierAimingSimulationData.ZoomLevels[0] -> ZoomLevelData.FieldOfView  (hip FOV, 55 vertical)
//   - LevelData.DefaultFOV                                                    (55)
//   - SoldierWeaponData.RenderFov  (FOV the weapon model is drawn with, 30;
//     a wider render FOV draws the weapon smaller)
//
// Values are patched in place (same size). No backups are needed: going back to
// the defaults simply writes 55 / 30 again.
package main

import (
	"bytes"
	"compress/gzip"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

const (
	defaultVFov      = 55.0 // game's vertical FOV
	defaultRenderFov = 30.0 // game's weapon model FOV
	fovStateVersion  = 8
)

// HorizontalToVertical converts a 16:9 horizontal FOV (what players know) to the
// vertical FOV the game stores.
func horizontalToVertical(h float64) float64 {
	return 2 * math.Atan(math.Tan(h*math.Pi/360)*9/16) * 180 / math.Pi
}

// weaponRenderFov returns the view model FOV for a size in percent (100 = original).
func weaponRenderFov(sizePct float64) float64 {
	if sizePct <= 0 {
		sizePct = 100
	}
	return 2 * math.Atan(math.Tan(defaultRenderFov*math.Pi/360)/(sizePct/100)) * 180 / math.Pi
}

type fovState struct {
	Version    int     `json:"version"`
	VFov       float32 `json:"vfov"`
	RenderFov  float32 `json:"render_fov"`
	LinearAim  bool    `json:"linear_aim"`
	SlotKeys   bool    `json:"slot_keys"`
	SprintZoom bool    `json:"sprint_zoom"`
	TankSpeed  int     `json:"tank_speed,omitempty"`
	AdsScale   bool    `json:"ads_scale,omitempty"`
	// Original turret thresholds replaced for mouse aiming, by "level|entry|offset".
	Turrets map[string]uint32 `json:"turrets,omitempty"`
}

func fovStatePath(gameData string) string { return filepath.Join(gameData, "bfbc_fov.json") }

func fovUpToDate(gameData string, vfov, rfov float32, linear, slots, sprintZoom bool, tankSpeed int) bool {
	var st fovState
	b, err := os.ReadFile(fovStatePath(gameData))
	if err != nil {
		return vfov == 0 && rfov == defaultRenderFov && !linear && !slots && sprintZoom && tankSpeed == 0 && !adsScaleOn
	}
	return json.Unmarshal(b, &st) == nil && st.Version == fovStateVersion && st.VFov == vfov &&
		st.RenderFov == rfov && st.LinearAim == linear && st.SlotKeys == slots && st.SprintZoom == sprintZoom &&
		st.TankSpeed == tankSpeed && st.AdsScale == adsScaleOn
}

// applyFov patches every level archive. progress(i, n) is called per archive.
func applyFov(gameData string, vfov, rfov float32, linear, slots, sprintZoom bool, tankSpeed int,
	progress func(i, n int)) error {
	turretSpeedPct = tankSpeed
	if fovUpToDate(gameData, vfov, rfov, linear, slots, sprintZoom, tankSpeed) {
		return nil
	}
	dist := filepath.Join(gameData, "dist")
	if !dirExists(dist) {
		dist = filepath.Join(gameData, "Dist")
	}
	var levels []string
	_ = filepath.WalkDir(dist, func(p string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() && strings.EqualFold(filepath.Base(p), "level-00.fbrb") {
			levels = append(levels, p)
		}
		return nil
	})
	if len(levels) == 0 {
		return errors.New("no level archives found")
	}
	var old fovState
	if b, err := os.ReadFile(fovStatePath(gameData)); err == nil {
		_ = json.Unmarshal(b, &old)
	}
	turrets := old.Turrets
	if turrets == nil {
		turrets = map[string]uint32{}
	}
	for i, p := range levels {
		if progress != nil {
			progress(i, len(levels))
		}
		level := filepath.Base(filepath.Dir(p))
		if err := patchLevelFov(p, level, vfov, rfov, linear, slots, sprintZoom, filepath.Join(gameData, "bfbc_backup"), turrets); err != nil {
			// keep what was recorded so far so originals are never lost
			b, _ := json.Marshal(fovState{Turrets: turrets})
			_ = os.WriteFile(fovStatePath(gameData), b, 0o644)
			return fmt.Errorf("%s: %w", level, err)
		}
	}
	b, _ := json.Marshal(fovState{Version: fovStateVersion, VFov: vfov, RenderFov: rfov, LinearAim: linear,
		SlotKeys: slots, SprintZoom: sprintZoom, TankSpeed: tankSpeed, AdsScale: adsScaleOn, Turrets: turrets})
	return os.WriteFile(fovStatePath(gameData), b, 0o644)
}

// Vehicle gunner/driver aim uses "RightThumbXSquared": a squared response that,
// with a mouse driving the emulated stick, turns small movements into nothing.
// Linear aim renames the action input to "RightThumbX" in the dbx string table,
// in place: the 'S' becomes a terminating NUL ("RightThumbX\0quared").
var (
	squaredNames = [][]byte{[]byte("RightThumbXSquared\x00"), []byte("RightThumbYSquared\x00")}
	linearNames  = [][]byte{[]byte("RightThumbX\x00quared\x00"), []byte("RightThumbY\x00quared\x00")}
)

func patchVehicleAim(b []byte, linear bool) bool {
	from, to := squaredNames, linearNames
	if !linear {
		from, to = linearNames, squaredNames
	}
	changed := false
	for i := range from {
		for {
			k := bytes.Index(b, from[i])
			if k < 0 {
				break
			}
			copy(b[k:], to[i])
			changed = true
		}
	}
	return changed
}

func patchLevelFov(path, level string, vfov, rfov float32, linear, slots, sprintZoom bool, backupDir string,
	turrets map[string]uint32) error {
	toc, payload, err := readArchiveFull(path)
	if err != nil {
		return err
	}
	ents, _, err := parseTOC(toc)
	if err != nil {
		return err
	}
	// On foot the camera uses the weapon's hip FOV times the level's
	// InfantryFOVMultiplier (0.67): the stored 55 is really ~37 on screen. The
	// sprint FOV is used as is. vfov is the wanted on-screen vertical FOV.
	mult := float32(0.67)
	for name, e := range ents {
		if strings.HasSuffix(name, ".dbx") && e.off+e.size <= len(payload) {
			if m := levelInfantryFovMult(payload[e.off : e.off+e.size]); m > 0.1 && m <= 2 {
				mult = m
				break
			}
		}
	}
	hip := float32(defaultVFov)
	real := float32(defaultVFov) * mult
	if vfov > 0 {
		hip = vfov / mult
		real = vfov
	}
	changed := false
	for name, e := range ents {
		if !strings.HasSuffix(name, ".dbx") || e.off+e.size > len(payload) {
			continue
		}
		b := payload[e.off : e.off+e.size]
		if strings.Contains(name, "vehicleinput") && patchVehicleAim(b, linear) {
			changed = true
		}
		if bytes.Contains(b, []byte("SoldierSprintSettingsData")) && patchSprintFov(b, level+"|"+name, real, defaultVFov*mult, vfov == 0, sprintZoom, turrets) {
			changed = true
		}
		if bytes.Contains(b, []byte("ChildRotationBodyData")) && patchTurretThreshold(b, level+"|"+name, linear, turrets) {
			changed = true
		}
		if bytes.Contains(b, []byte("ZoomLevels")) && patchAdsFov(b, level+"|"+name, real, defaultVFov*mult, mult, turrets) {
			changed = true
		}
		if !bytes.Contains(b, []byte("RenderFov")) && !bytes.Contains(b, []byte("ZoomLevels")) &&
			!bytes.Contains(b, []byte("DefaultFOV")) {
			continue
		}
		targets, err := dbxFovTargets(b)
		if err != nil {
			continue // not a binary dbx we understand; leave it alone
		}
		for _, t := range targets {
			v := hip
			if t.render {
				v = rfov
			}
			if t.fixed > 0 {
				v = t.fixed
			}
			var cur [4]byte
			copy(cur[:], b[t.off:t.off+4])
			binary.BigEndian.PutUint32(b[t.off:t.off+4], math.Float32bits(v))
			if !bytes.Equal(cur[:], b[t.off:t.off+4]) {
				changed = true
			}
		}
	}
	// Weapon slot keys (soldier input map). The original is kept in backupDir.
	replace := map[string][]byte{}
	backup := filepath.Join(backupDir, level+".defaultsoldierinput.dbx")
	if cur := archiveEntry(toc, payload, soldierInputEntry); cur != nil {
		if slots && !fileExists(backup) {
			if nb := addSlotActions(cur); nb != nil {
				if err := os.MkdirAll(backupDir, 0o755); err != nil {
					return err
				}
				if err := os.WriteFile(backup, cur, 0o644); err != nil {
					return err
				}
				replace[soldierInputEntry] = nb
			}
		} else if !slots && fileExists(backup) {
			orig, err := os.ReadFile(backup)
			if err != nil {
				return err
			}
			replace[soldierInputEntry] = orig
		}
	}
	if !changed && len(replace) == 0 {
		return nil
	}
	if err := writeArchive(path, toc, payload, replace); err != nil {
		return err
	}
	if !slots && len(replace) > 0 {
		_ = os.Remove(backup)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Binary dbx ("{binary}") reader - just enough to find the FOV fields.
//   header: "{binary}" u32 treeOffset, u32 0, u32 ?, u32 stringCount, u32 offsets[], strings
//   node:   varint tag, u8 flags (0x80 children, 0x40 binary value, 0x20 value,
//           0x10 counted binary value, low 4 bits attribute count),
//           attrs (varint key, varint value), value, children..., 0
// ---------------------------------------------------------------------------

type dbxNode struct {
	tag      string
	attrs    map[string]string
	text     string
	valOff   int
	valLen   int
	children []*dbxNode
}

type fovTarget struct {
	off    int
	render bool
	fixed  float32 // >0: always write this value
}

func parseDbx(b []byte) (*dbxNode, error) {
	if len(b) < 24 || string(b[:8]) != "{binary}" {
		return nil, errors.New("not binary dbx")
	}
	cnt := int(binary.BigEndian.Uint32(b[20:24]))
	if 24+4*cnt > len(b) {
		return nil, errors.New("bad dbx")
	}
	base := 24 + 4*cnt
	strs := make([]string, cnt)
	maxEnd := base
	for i := 0; i < cnt; i++ {
		o := base + int(binary.BigEndian.Uint32(b[24+4*i:]))
		if o >= len(b) {
			return nil, errors.New("bad string")
		}
		e := bytes.IndexByte(b[o:], 0)
		if e < 0 {
			return nil, errors.New("bad string")
		}
		strs[i] = string(b[o : o+e])
		if o+e+1 > maxEnd {
			maxEnd = o + e + 1
		}
	}
	p := maxEnd
	var errBad = errors.New("bad dbx tree")
	vi := func() (int, error) {
		r, sh := 0, 0
		for {
			if p >= len(b) || sh > 28 {
				return 0, errBad
			}
			c := b[p]
			p++
			r |= int(c&0x7f) << sh
			sh += 7
			if c < 0x80 {
				return r, nil
			}
		}
	}
	str := func() (string, error) {
		i, err := vi()
		if err != nil || i >= len(strs) {
			return "", errBad
		}
		return strs[i], nil
	}
	var node func(depth int) (*dbxNode, error)
	node = func(depth int) (*dbxNode, error) {
		if depth > 64 {
			return nil, errBad
		}
		n := &dbxNode{attrs: map[string]string{}, valOff: -1}
		var err error
		if n.tag, err = str(); err != nil {
			return nil, err
		}
		if p >= len(b) {
			return nil, errBad
		}
		f := b[p]
		p++
		for i := 0; i < int(f&0x0f); i++ {
			k, err := str()
			if err != nil {
				return nil, err
			}
			v, err := str()
			if err != nil {
				return nil, err
			}
			n.attrs[k] = v
		}
		if f&0x20 != 0 {
			if f&0x40 != 0 {
				ln := 0
				if f&0x10 != 0 {
					c, err := vi()
					if err != nil {
						return nil, err
					}
					es, err := vi()
					if err != nil {
						return nil, err
					}
					ln = c * es
				} else if ln, err = vi(); err != nil {
					return nil, err
				}
				if p+ln > len(b) {
					return nil, errBad
				}
				n.valOff, n.valLen = p, ln
				p += ln
			} else if n.text, err = str(); err != nil {
				return nil, err
			}
		}
		if f&0x80 != 0 {
			for {
				if p >= len(b) {
					return nil, errBad
				}
				if b[p] == 0 {
					p++
					break
				}
				c, err := node(depth + 1)
				if err != nil {
					return nil, err
				}
				n.children = append(n.children, c)
			}
		}
		return n, nil
	}
	return node(0)
}

func (n *dbxNode) field(name string) *dbxNode {
	for _, c := range n.children {
		if c.attrs["name"] == name {
			return c
		}
	}
	return nil
}

func dbxFovTargets(b []byte) ([]fovTarget, error) {
	root, err := parseDbx(b)
	if err != nil {
		return nil, err
	}
	byGUID := map[string]*dbxNode{}
	for _, in := range root.children {
		byGUID[in.attrs["guid"]] = in
	}
	var out []fovTarget
	add := func(f *dbxNode, render bool) {
		if f != nil && f.valOff >= 0 && f.valLen == 4 {
			out = append(out, fovTarget{off: f.valOff, render: render})
		}
	}
	for _, in := range root.children {
		t := in.attrs["type"]
		switch {
		case strings.HasSuffix(t, ".SoldierWeaponData"):
			add(in.field("RenderFov"), true)
		case strings.HasSuffix(t, ".LevelData"):
			// DefaultFOV is left alone: the terrain uses it for its level of detail
			// and breaks (stretched spikes) when it changes. It is still restored
			// here if an older launcher changed it.
			if f := in.field("DefaultFOV"); f != nil && f.valOff >= 0 && f.valLen == 4 {
				out = append(out, fovTarget{off: f.valOff, fixed: defaultVFov})
			}
		}
		if zl := in.field("ZoomLevels"); zl != nil && zl.tag == "array" && len(zl.children) > 0 {
			if z := byGUID[zl.children[0].attrs["ref"]]; z != nil {
				add(z.field("FieldOfView"), false)
			}
		}
	}
	return out, nil
}

// Vehicle turrets and mounted guns are physics bodies turned by torque from the
// aim input. With UseAngularMomentumThreshold the torque must exceed a threshold
// before anything moves, so small mouse movements never turn the gun. For mouse
// aiming the threshold is lowered to almost nothing; the originals are kept in
// bfbc_fov.json and written back for gamepads.
const tinyThreshold = 0.001

// turretSpeedPct scales heavy turret torque (0 or 100 = original); set by applyFov.
var turretSpeedPct int

// adsScaleOn: scale aim-down-sights FOVs with the chosen FOV; set from the settings.
var adsScaleOn bool

// patchAdsFov scales the aiming zoom levels (ZoomLevels[1:]) so aiming magnifies
// as much as in the original game relative to the hip FOV, instead of zooming
// far more when the hip FOV is widened. Scaling is done on tan(fov/2), on the real
// (InfantryFOVMultiplier-applied) angles. Sniper scopes (stored FOV < 20) keep
// their fixed magnification. Originals are kept in orig and restored when off.
func patchAdsFov(b []byte, key string, real, defaultReal, mult float32, orig map[string]uint32) bool {
	root, err := parseDbx(b)
	if err != nil {
		return false
	}
	byGUID := map[string]*dbxNode{}
	for _, in := range root.children {
		byGUID[in.attrs["guid"]] = in
	}
	k := math.Tan(float64(real)*math.Pi/360) / math.Tan(float64(defaultReal)*math.Pi/360)
	changed := false
	for _, in := range root.children {
		zl := in.field("ZoomLevels")
		if zl == nil || zl.tag != "array" || len(zl.children) < 2 {
			continue
		}
		for _, ref := range zl.children[1:] {
			z := byGUID[ref.attrs["ref"]]
			if z == nil {
				continue
			}
			f := z.field("FieldOfView")
			if f == nil || f.valOff < 0 || f.valLen != 4 {
				continue
			}
			mk := fmt.Sprintf("%s|ads|%d", key, f.valOff)
			cur := binary.BigEndian.Uint32(b[f.valOff:])
			base := cur
			if o, ok := orig[mk]; ok {
				base = o
			}
			stored := math.Float32frombits(base)
			want := base
			if adsScaleOn && k != 1 && stored >= 20 && stored < 179 {
				orig[mk] = base
				r := float64(stored*mult) * math.Pi / 360
				nr := 2 * math.Atan(math.Tan(r)*k) * 180 / math.Pi
				want = math.Float32bits(float32(nr) / mult)
			} else {
				delete(orig, mk)
			}
			if cur != want {
				binary.BigEndian.PutUint32(b[f.valOff:], want)
				changed = true
			}
		}
	}
	return changed
}

func patchTurretThreshold(b []byte, key string, linear bool, orig map[string]uint32) bool {
	root, err := parseDbx(b)
	if err != nil {
		return false
	}
	changed := false
	for _, in := range root.children {
		if !strings.HasSuffix(in.attrs["type"], ".ChildRotationBodyData") {
			continue
		}
		use, inp, thr := in.field("UseAngularMomentumThreshold"), in.field("RotationInput"), in.field("AngularMomentumThreshold")
		if use == nil || inp == nil || thr == nil || use.valOff < 0 || inp.valOff < 0 || thr.valOff < 0 ||
			use.valLen != 1 || inp.valLen != 4 || thr.valLen != 4 {
			continue
		}
		axis := binary.BigEndian.Uint32(b[inp.valOff:])
		if b[use.valOff] != 1 || (axis != 6 && axis != 7) { // 6/7 = aim yaw/pitch
			continue
		}
		// Heavy turrets (tank main gun, AA gun: inertia >= 1000, small torque) turn
		// slowly by design; optionally scale their torque.
		inr, mul := in.field("Inertia"), in.field("AngularMomentumMultiplier")
		if inr != nil && mul != nil && inr.valOff >= 0 && mul.valOff >= 0 && inr.valLen == 4 && mul.valLen == 4 {
			mk := fmt.Sprintf("%s|%d", key, mul.valOff)
			cur := binary.BigEndian.Uint32(b[mul.valOff:])
			base := cur
			if o, ok := orig[mk]; ok {
				base = o
			}
			inertia := math.Float32frombits(binary.BigEndian.Uint32(b[inr.valOff:]))
			bm := math.Float32frombits(base)
			if turretSpeedPct > 100 && inertia >= 1000 && bm > 0 && bm < 100 {
				orig[mk] = base
				want := math.Float32bits(bm * float32(turretSpeedPct) / 100)
				if cur != want {
					binary.BigEndian.PutUint32(b[mul.valOff:], want)
					changed = true
				}
			} else if o, ok := orig[mk]; ok {
				if cur != o {
					binary.BigEndian.PutUint32(b[mul.valOff:], o)
					changed = true
				}
				delete(orig, mk)
			}
		}
		// UseLinearInput: the aim input drives the rotation directly instead of as a
		// torque on a physics body (no inertia, no momentum threshold).
		if lin := in.field("UseLinearInput"); lin != nil && lin.valOff >= 0 && lin.valLen == 1 {
			lk := fmt.Sprintf("%s|%d", key, lin.valOff)
			if linear {
				if _, done := orig[lk]; !done {
					orig[lk] = uint32(b[lin.valOff])
				}
				if b[lin.valOff] != 1 {
					b[lin.valOff] = 1
					changed = true
				}
			} else if o, ok := orig[lk]; ok {
				if b[lin.valOff] != byte(o) {
					b[lin.valOff] = byte(o)
					changed = true
				}
				delete(orig, lk)
			}
		}
		k := fmt.Sprintf("%s|%d", key, thr.valOff)
		cur := binary.BigEndian.Uint32(b[thr.valOff:])
		if linear {
			if _, done := orig[k]; !done {
				orig[k] = cur
			}
			want := math.Float32bits(tinyThreshold)
			if cur != want {
				binary.BigEndian.PutUint32(b[thr.valOff:], want)
				changed = true
			}
		} else if o, ok := orig[k]; ok {
			if cur != o {
				binary.BigEndian.PutUint32(b[thr.valOff:], o)
				changed = true
			}
			delete(orig, k)
		}
	}
	return changed
}

// ---------------------------------------------------------------------------
// Weapon slot keys: the soldier input map (DefaultSoldierInput) binds the engine
// actions SelectWeapon1..5 by position (Actions[30..34]); the console data only
// has SelectWeapon1 (Y). We append SelectWeapon2..5 bound to the D-pad, which the
// game build maps to keys 2-5.
// ---------------------------------------------------------------------------

const soldierInputEntry = "objects/soldiers/common/defaultsoldierinput.dbx"

var slotDpad = []string{"DpadUp", "DpadDown", "DpadLeft", "DpadRight"}

type dbxRaw struct {
	b     []byte
	strs  []string
	offs  []uint32
	tree  int
	index map[string]int
}

type dbxPos struct {
	tag, typ, name string
	attrs          map[string]string
	text           string
	endChildren    int
	children       []*dbxPos
}

func vEnc(v int) []byte {
	var o []byte
	for {
		c := byte(v & 0x7f)
		v >>= 7
		if v != 0 {
			o = append(o, c|0x80)
		} else {
			return append(o, c)
		}
	}
}

// parseDbxPos parses the tree and records where each node's child list ends.
func parseDbxPos(b []byte) (*dbxRaw, *dbxPos, error) {
	if len(b) < 24 || string(b[:8]) != "{binary}" {
		return nil, nil, errors.New("not binary dbx")
	}
	r := &dbxRaw{b: b, index: map[string]int{}}
	r.tree = int(binary.BigEndian.Uint32(b[8:12]))
	cnt := int(binary.BigEndian.Uint32(b[20:24]))
	base := 24 + 4*cnt
	if base > len(b) || r.tree > len(b) {
		return nil, nil, errors.New("bad dbx")
	}
	for i := 0; i < cnt; i++ {
		o := binary.BigEndian.Uint32(b[24+4*i:])
		r.offs = append(r.offs, o)
		s := base + int(o)
		e := bytes.IndexByte(b[s:], 0)
		if e < 0 {
			return nil, nil, errors.New("bad string")
		}
		str := string(b[s : s+e])
		r.strs = append(r.strs, str)
		if _, ok := r.index[str]; !ok {
			r.index[str] = i
		}
	}
	p := r.tree
	bad := errors.New("bad dbx tree")
	vi := func() (int, error) {
		v, sh := 0, 0
		for {
			if p >= len(b) || sh > 28 {
				return 0, bad
			}
			c := b[p]
			p++
			v |= int(c&0x7f) << sh
			sh += 7
			if c < 0x80 {
				return v, nil
			}
		}
	}
	str := func() (string, error) {
		i, err := vi()
		if err != nil || i >= len(r.strs) {
			return "", bad
		}
		return r.strs[i], nil
	}
	var node func(d int) (*dbxPos, error)
	node = func(d int) (*dbxPos, error) {
		if d > 64 {
			return nil, bad
		}
		n := &dbxPos{attrs: map[string]string{}, endChildren: -1}
		var err error
		if n.tag, err = str(); err != nil {
			return nil, err
		}
		if p >= len(b) {
			return nil, bad
		}
		f := b[p]
		p++
		for i := 0; i < int(f&0x0f); i++ {
			k, err := str()
			if err != nil {
				return nil, err
			}
			v, err := str()
			if err != nil {
				return nil, err
			}
			n.attrs[k] = v
		}
		if f&0x20 != 0 {
			if f&0x40 != 0 {
				ln := 0
				if f&0x10 != 0 {
					c, err := vi()
					if err != nil {
						return nil, err
					}
					es, err := vi()
					if err != nil {
						return nil, err
					}
					ln = c * es
				} else if ln, err = vi(); err != nil {
					return nil, err
				}
				p += ln
			} else if n.text, err = str(); err != nil {
				return nil, err
			}
		}
		if f&0x80 != 0 {
			for {
				if p >= len(b) {
					return nil, bad
				}
				if b[p] == 0 {
					n.endChildren = p
					p++
					break
				}
				c, err := node(d + 1)
				if err != nil {
					return nil, err
				}
				n.children = append(n.children, c)
			}
		}
		return n, nil
	}
	p = r.tree
	root, err := node(0)
	return r, root, err
}

// addSlotActions returns a new DefaultSoldierInput with SelectWeapon2..5 added,
// or nil if the file does not have the expected layout (31 actions per map).
func addSlotActions(b []byte) []byte {
	r, root, err := parseDbxPos(b)
	if err != nil {
		return nil
	}
	var added []string
	S := func(s string) []byte {
		i, ok := r.index[s]
		if !ok {
			i = len(r.strs) + len(added)
			r.index[s] = i
			added = append(added, s)
		}
		return vEnc(i)
	}
	type ins struct {
		pos  int
		data []byte
	}
	var inserts []ins
	var inst []byte
	k := 0
	for _, in := range root.children {
		if !strings.HasSuffix(in.attrs["type"], ".EntryInputActionMapData") {
			continue
		}
		var arr *dbxPos
		for _, c := range in.children {
			if c.tag == "array" && c.attrs["name"] == "Actions" {
				arr = c
			}
		}
		if arr == nil || len(arr.children) != 31 || arr.endChildren < 0 {
			return nil
		}
		var items []byte
		for _, d := range slotDpad {
			g1 := fmt.Sprintf("B0BC5107-0000-4000-8000-%012X", 2*k)
			g2 := fmt.Sprintf("B0BC5107-0000-4000-8000-%012X", 2*k+1)
			k++
			items = append(items, S("item")...)
			items = append(items, 0x21)
			items = append(append(append(items, S("ref")...), S(g1)...), 0)
			inst = append(inst, S("instance")...)
			inst = append(inst, 0xa2)
			inst = append(append(inst, S("guid")...), S(g1)...)
			inst = append(append(inst, S("type")...), S("GameSharedResources.InputActionsData")...)
			inst = append(inst, 0)
			inst = append(inst, S("array")...)
			inst = append(inst, 0xa1)
			inst = append(append(inst, S("name")...), S("InputActions")...)
			inst = append(inst, 0)
			inst = append(inst, S("item")...)
			inst = append(inst, 0x21)
			inst = append(append(append(inst, S("ref")...), S(g2)...), 0, 0, 0)
			inst = append(inst, S("instance")...)
			inst = append(inst, 0xa2)
			inst = append(append(inst, S("guid")...), S(g2)...)
			inst = append(append(inst, S("type")...), S("GameSharedResources.InputActionData")...)
			inst = append(inst, 0)
			inst = append(inst, S("field")...)
			inst = append(inst, 0x21)
			inst = append(append(append(inst, S("name")...), S("ActionName")...), S(d)...)
			inst = append(inst, 0)
		}
		inserts = append(inserts, ins{arr.endChildren, items})
	}
	if len(inserts) == 0 || root.endChildren < 0 {
		return nil
	}
	inserts = append(inserts, ins{root.endChildren, inst})
	sort.Slice(inserts, func(i, j int) bool { return inserts[i].pos > inserts[j].pos })
	tree := append([]byte(nil), b[r.tree:]...)
	for _, in := range inserts {
		at := in.pos - r.tree
		tree = append(tree[:at], append(append([]byte(nil), in.data...), tree[at:]...)...)
	}
	cnt := len(r.offs)
	pool := append([]byte(nil), b[24+4*cnt:r.tree]...)
	offs := append([]uint32(nil), r.offs...)
	for _, s := range added {
		offs = append(offs, uint32(len(pool)))
		pool = append(append(pool, s...), 0)
	}
	n := len(offs)
	newTree := 24 + 4*n + len(pool)
	out := make([]byte, 0, newTree+len(tree))
	out = append(out, "{binary}"...)
	out = binary.BigEndian.AppendUint32(out, uint32(newTree))
	out = binary.BigEndian.AppendUint32(out, 0)
	out = binary.BigEndian.AppendUint32(out, uint32(newTree-24))
	out = binary.BigEndian.AppendUint32(out, uint32(n))
	for _, o := range offs {
		out = binary.BigEndian.AppendUint32(out, o)
	}
	out = append(out, pool...)
	return append(out, tree...)
}

// rebuildArchive replaces whole entries (sizes may change) and rewrites the TOC.
//
//	TOC: u32 version, u32 strLen, strings, u32 count, count*{nameOff, flags,
//	     offset, size, size, typeOff}, u8 1, u32 payloadSize
func readArchiveFull(path string) (toc, payload []byte, err error) {
	d, err := os.ReadFile(path)
	if err != nil {
		return nil, nil, err
	}
	if len(d) < 8 || string(d[:4]) != "FbRB" {
		return nil, nil, errors.New("not fbrb")
	}
	n := int(binary.BigEndian.Uint32(d[4:8]))
	if 8+n > len(d) {
		return nil, nil, errors.New("truncated")
	}
	if toc, err = gunzip(d[8 : 8+n]); err != nil {
		return nil, nil, err
	}
	payload, err = gunzip(d[8+n:])
	return toc, payload, err
}

// writeArchive writes toc+payload, replacing whole entries (sizes may change).
func writeArchive(path string, toc, payload []byte, replace map[string][]byte) error {
	slen := int(binary.BigEndian.Uint32(toc[4:8]))
	strs := toc[8 : 8+slen]
	rest := 8 + slen
	cnt := int(binary.BigEndian.Uint32(toc[rest:]))
	tail := rest + 4 + cnt*24
	if tail+5 > len(toc) || toc[tail] != 1 || int(binary.BigEndian.Uint32(toc[tail+1:])) != len(payload) {
		return errors.New("unexpected archive layout")
	}
	name := func(o int) string {
		e := bytes.IndexByte(strs[o:], 0)
		return strings.ToLower(string(strs[o : o+e]))
	}
	type ent struct{ i, off, size int }
	ents := make([]ent, cnt)
	for i := 0; i < cnt; i++ {
		e := toc[rest+4+i*24:]
		ents[i] = ent{i, int(binary.BigEndian.Uint32(e[8:])), int(binary.BigEndian.Uint32(e[12:]))}
	}
	order := append([]ent(nil), ents...)
	sort.Slice(order, func(a, b int) bool { return order[a].off < order[b].off })
	var np bytes.Buffer
	newOff := make([]int, cnt)
	newSize := make([]int, cnt)
	for _, e := range order {
		data := payload[e.off : e.off+e.size]
		nm := name(int(binary.BigEndian.Uint32(toc[rest+4+e.i*24:])))
		if r, ok := replace[nm]; ok {
			data = r
		}
		newOff[e.i] = np.Len()
		newSize[e.i] = len(data)
		np.Write(data)
	}
	nt := append([]byte(nil), toc...)
	for i := 0; i < cnt; i++ {
		e := nt[rest+4+i*24:]
		binary.BigEndian.PutUint32(e[8:], uint32(newOff[i]))
		binary.BigEndian.PutUint32(e[12:], uint32(newSize[i]))
		binary.BigEndian.PutUint32(e[16:], uint32(newSize[i]))
	}
	binary.BigEndian.PutUint32(nt[tail+1:], uint32(np.Len()))
	var tz bytes.Buffer
	zw, _ := gzip.NewWriterLevel(&tz, gzip.BestSpeed)
	zw.Write(nt)
	if err := zw.Close(); err != nil {
		return err
	}
	var out bytes.Buffer
	out.WriteString("FbRB")
	_ = binary.Write(&out, binary.BigEndian, uint32(tz.Len()))
	out.Write(tz.Bytes())
	zw2, _ := gzip.NewWriterLevel(&out, gzip.BestSpeed)
	if _, err := zw2.Write(np.Bytes()); err != nil {
		return err
	}
	if err := zw2.Close(); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, out.Bytes(), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// archiveEntry returns the bytes of a named entry.
func archiveEntry(toc, payload []byte, want string) []byte {
	ents, _, err := parseTOC(toc)
	if err != nil {
		return nil
	}
	e, ok := ents[want]
	if !ok || e.off+e.size > len(payload) {
		return nil
	}
	return payload[e.off : e.off+e.size]
}

// Sprinting narrows the view (single player: 36 instead of 55). Without sprint
// zoom the sprint FOV equals the normal FOV; with it the original ratio is kept
// relative to the chosen FOV. Originals are kept in the state file.
func patchSprintFov(b []byte, key string, real, defaultReal float32, isDefault, sprintZoom bool,
	orig map[string]uint32) bool {
	root, err := parseDbx(b)
	if err != nil {
		return false
	}
	changed := false
	for _, in := range root.children {
		if !strings.HasSuffix(in.attrs["type"], ".SoldierSprintSettingsData") {
			continue
		}
		f := in.field("Fov")
		if f == nil || f.valOff < 0 || f.valLen != 4 {
			continue
		}
		k := fmt.Sprintf("%s|%d", key, f.valOff)
		cur := binary.BigEndian.Uint32(b[f.valOff:])
		o, ok := orig[k]
		if !ok {
			o = cur
		}
		wb := math.Float32bits(real) // no sprint zoom: same view as walking
		if sprintZoom {
			wb = math.Float32bits(math.Float32frombits(o) * real / defaultReal)
		}
		if sprintZoom && isDefault {
			wb = o // fully original: forget the record
			delete(orig, k)
		} else {
			orig[k] = o
		}
		if cur != wb {
			binary.BigEndian.PutUint32(b[f.valOff:], wb)
			changed = true
		}
	}
	return changed
}

// levelInfantryFovMult returns LevelData.InfantryFOVMultiplier, or 0.
func levelInfantryFovMult(b []byte) float32 {
	if !bytes.Contains(b, []byte("InfantryFOVMultiplier")) {
		return 0
	}
	root, err := parseDbx(b)
	if err != nil {
		return 0
	}
	for _, in := range root.children {
		if strings.HasSuffix(in.attrs["type"], ".LevelData") {
			if f := in.field("InfantryFOVMultiplier"); f != nil && f.valOff >= 0 && f.valLen == 4 {
				return math.Float32frombits(binary.BigEndian.Uint32(b[f.valOff:]))
			}
		}
	}
	return 0
}
