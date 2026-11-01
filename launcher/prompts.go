// In-game button prompts: Xbox (original), PlayStation or keyboard/mouse.
//
// The game's UI (EA APT movies) draws controller glyphs from texture atlases stored
// in .fbrb archives. We repaint the Xbox glyph sprites inside those atlases:
//   - PlayStation: the game's own PS3 glyphs (also present in hud_root) are copied over.
//   - Keyboard/mouse: keycaps with the user's bound key names are drawn.
//
// Originals are kept as <archive>.orig and restored when Xbox is selected.
package main

import (
	"bytes"
	"compress/gzip"
	"crypto/sha1"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/color"
	"io"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"golang.org/x/image/draw"
	"golang.org/x/image/font"
	"golang.org/x/image/font/gofont/gobold"
	"golang.org/x/image/font/opentype"
	"golang.org/x/image/math/fixed"
)

const promptsVersion = 1

type rect struct{ X0, Y0, X1, Y1 int }

func (r rect) W() int               { return r.X1 - r.X0 }
func (r rect) H() int               { return r.Y1 - r.Y0 }
func (r rect) img() image.Rectangle { return image.Rect(r.X0, r.Y0, r.X1, r.Y1) }

type glyph struct {
	Btn string // A B X Y LB RB LT RT LS RS START BACK DPAD
	R   rect
	Alt bool // use the alternate (darker) PS circle
	// Mono: white single-colour glyph embedded in a text sprite (igm_sp "(A) SELECT").
	Mono bool
}

type atlasSpec struct {
	Entry  string // texture entry name (lower case)
	W, H   int
	Fmt    uint32
	Glyphs []glyph
}

const hudRootEntry = "menu/hud_root_textures_xenon/atlas0.res"

// Xbox glyph sprites per atlas (from each atlas_meta.dbx, verified by eye).
var atlasSpecs = []atlasSpec{
	{"menu/menu_common_textures_xenon/atlas0.res", 256, 256, 9, []glyph{
		{Btn: "A", R: rect{43, 170, 77, 204}}, {Btn: "B", R: rect{148, 32, 182, 66}},
		{Btn: "X", R: rect{164, 66, 198, 100}}, {Btn: "Y", R: rect{182, 0, 216, 34}},
		{Btn: "LB", R: rect{119, 77, 164, 103}}, {Btn: "RB", R: rect{0, 142, 45, 168}},
		{Btn: "LT", R: rect{121, 32, 148, 77}}, {Btn: "RT", R: rect{79, 103, 106, 148}},
		{Btn: "RS", R: rect{0, 107, 42, 142}}, {Btn: "START", R: rect{120, 0, 160, 32}},
		{Btn: "BACK", R: rect{79, 71, 119, 103}},
	}},
	{hudRootEntry, 512, 1024, 2, []glyph{
		{Btn: "START", R: rect{90, 824, 130, 856}}, {Btn: "BACK", R: rect{228, 785, 268, 817}},
		{Btn: "Y", R: rect{0, 864, 34, 898}}, {Btn: "A", R: rect{93, 856, 127, 890}},
		{Btn: "X", R: rect{130, 855, 164, 889}}, {Btn: "B", R: rect{313, 801, 347, 835}},
		{Btn: "DPAD", R: rect{89, 776, 129, 816}},
		{Btn: "LT", R: rect{485, 389, 512, 434}}, {Btn: "LB", R: rect{268, 785, 313, 811}},
		{Btn: "RT", R: rect{376, 737, 403, 782}}, {Btn: "RB", R: rect{171, 826, 216, 852}},
		{Btn: "LS", R: rect{394, 670, 436, 706}}, {Btn: "RS", R: rect{440, 644, 482, 680}},
		{Btn: "B", R: rect{216, 844, 248, 876}, Alt: true}, {Btn: "B", R: rect{364, 782, 398, 816}, Alt: true},
	}},
	{"menu/hud_popups_textures_xenon/atlas0.res", 256, 512, 2, []glyph{
		{Btn: "A", R: rect{68, 200, 102, 234}}, {Btn: "Y", R: rect{34, 200, 68, 234}}, {Btn: "B", R: rect{0, 200, 34, 234}},
	}},
	{"menu/main_popups_textures_xenon/atlas0.res", 1024, 512, 9, []glyph{
		{Btn: "A", R: rect{68, 416, 102, 450}}, {Btn: "Y", R: rect{0, 416, 34, 450}}, {Btn: "B", R: rect{320, 215, 354, 249}},
	}},
	{"menu/main_nav_textures_xenon/atlas0.res", 2048, 1024, 9, []glyph{
		{Btn: "A", R: rect{1503, 606, 1537, 640}}, {Btn: "Y", R: rect{1635, 22, 1669, 56}}, {Btn: "B", R: rect{1556, 481, 1590, 515}},
	}},
	{"menu/loading_mp_textures_xenon/atlas0.res", 1024, 1024, 2, []glyph{
		{Btn: "A", R: rect{434, 509, 468, 543}},
	}},
	{"menu/loading_sp_textures_xenon/atlas0.res", 64, 128, 2, []glyph{
		{Btn: "LB", R: rect{0, 0, 45, 26}}, {Btn: "RB", R: rect{0, 26, 45, 52}},
	}},
	{"menu/igm_mp_textures_xenon/atlas0.res", 512, 1024, 2, []glyph{
		{Btn: "LB", R: rect{397, 526, 442, 552}}, {Btn: "RB", R: rect{442, 487, 487, 513}},
	}},
	{"menu/igm_sp_textures_xenon/atlas0.res", 512, 256, 2, []glyph{
		{Btn: "A", R: rect{98, 163, 112, 177}, Mono: true}, {Btn: "B", R: rect{46, 235, 60, 249}, Mono: true},
	}},
}

// PlayStation glyphs inside hud_root (the game ships both sets).
var psSource = map[string]rect{
	"A": {403, 737, 437, 771}, "B": {437, 727, 471, 761}, "B_ALT": {471, 717, 505, 751},
	"X": {59, 856, 93, 890}, "Y": {268, 811, 302, 845},
	"LB": {216, 817, 259, 844}, "RB": {465, 690, 508, 717},
	"LT": {185, 754, 228, 792}, "RT": {46, 776, 89, 814},
	"LS": {139, 754, 185, 792}, "RS": {348, 666, 394, 704},
	"START": {219, 648, 274, 684}, "BACK": {319, 509, 385, 544}, "DPAD": {274, 647, 318, 691},
}

// Which keybind cvar is shown for each Xbox button in keyboard mode.
var promptKeybind = map[string]string{
	"A": "keybind_a", "B": "keybind_b", "X": "keybind_x", "Y": "keybind_y",
	"LB": "keybind_left_shoulder", "RB": "keybind_right_shoulder",
	"LT": "keybind_left_trigger", "RT": "keybind_right_trigger",
	"LS": "keybind_lstick_press", "RS": "keybind_rstick_press",
	"START": "keybind_start", "BACK": "keybind_back",
}

// ---------------------------------------------------------------------------
// Entry point
// ---------------------------------------------------------------------------

type promptsState struct {
	Version int      `json:"version"`
	Sig     string   `json:"sig"`
	Files   []string `json:"files"`
}

func promptsStatePath(gameData string) string {
	return filepath.Join(gameData, "bfbc_button_prompts.json")
}

// keyLabels returns the label drawn for each button in keyboard mode.
func keyLabels(binds map[string]string) map[string]string {
	out := map[string]string{}
	for btn, cv := range promptKeybind {
		out[btn] = shortKeyName(binds[cv])
	}
	d := []string{shortKeyName(binds["keybind_dpad_up"]), shortKeyName(binds["keybind_dpad_down"]),
		shortKeyName(binds["keybind_dpad_left"]), shortKeyName(binds["keybind_dpad_right"])}
	if d[0] == "1" && d[1] == "2" && d[2] == "3" && d[3] == "4" {
		out["DPAD"] = "1-4"
	} else if len(d[0]) == 1 && len(d[1]) == 1 && len(d[2]) == 1 && len(d[3]) == 1 {
		out["DPAD"] = d[0] + d[1] + d[2] + d[3]
	} else {
		out["DPAD"] = d[0]
	}
	return out
}

func shortKeyName(k string) string {
	k = strings.TrimSpace(k)
	m := map[string]string{
		"space": "SPACE", "escape": "ESC", "esc": "ESC", "return": "ENTER", "enter": "ENTER",
		"shift": "SHIFT", "lshift": "SHIFT", "rshift": "SHIFT", "control": "CTRL", "ctrl": "CTRL",
		"lcontrol": "CTRL", "rcontrol": "CTRL", "alt": "ALT", "lalt": "ALT", "ralt": "ALT",
		"tab": "TAB", "backspace": "BKSP", "back": "BKSP", "delete": "DEL", "insert": "INS",
		"capslock": "CAPS", "up": "↑", "down": "↓", "left": "←", "right": "→",
		"pageup": "PGUP", "pagedown": "PGDN", "home": "HOME", "end": "END",
		"lmb": "LMB", "rmb": "RMB", "mmb": "MMB", "mouse4": "M4", "mouse5": "M5",
		"wheelup": "WH↑", "wheeldown": "WH↓", "plus": "+", "minus": "-", "comma": ",", "period": ".",
		"semicolon": ";", "slash": "/", "backslash": "\\", "lbracket": "[", "rbracket": "]", "quote": "'",
		"backtick": "`", "numpadenter": "ENTER", "numpadplus": "N+", "numpadminus": "N-", "numpadstar": "N*",
		"numpadslash": "N/",
	}
	if strings.HasPrefix(strings.ToLower(k), "numpad") && len(k) == 7 {
		return "N" + k[6:]
	}
	if v, ok := m[strings.ToLower(strings.ReplaceAll(k, " ", ""))]; ok {
		return v
	}
	if k == "" {
		return "?"
	}
	k = strings.ToUpper(k)
	if r := []rune(k); len(r) > 5 {
		k = string(r[:5])
	}
	return k
}

func promptsSignature(mode string, binds map[string]string) string {
	sig := fmt.Sprintf("v%d|%s", promptsVersion, mode)
	if mode == "kbm" {
		l := keyLabels(binds)
		keys := make([]string, 0, len(l))
		for k := range l {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			sig += "|" + k + "=" + l[k]
		}
	}
	h := sha1.Sum([]byte(sig))
	return hex.EncodeToString(h[:8])
}

func normalizePromptMode(mode string) string {
	if mode != "ps" && mode != "kbm" {
		return "xbox"
	}
	return mode
}

// promptsUpToDate reports whether the archives already match mode/binds.
func promptsUpToDate(gameData, mode string, binds map[string]string) bool {
	mode = normalizePromptMode(mode)
	var st promptsState
	b, err := os.ReadFile(promptsStatePath(gameData))
	if err != nil {
		// Never patched: Xbox needs nothing unless a backup was left behind (test patch).
		return mode == "xbox" && !fileExists(filepath.Join(gameData, "dist", "xenon", "menu", "hud", "hud_singleplayer-00.fbrb.orig"))
	}
	if json.Unmarshal(b, &st) != nil || st.Version != promptsVersion || st.Sig != promptsSignature(mode, binds) {
		return false
	}
	for _, f := range st.Files {
		if !fileExists(f) || !fileExists(f+".orig") {
			return false
		}
	}
	return true
}

// applyButtonPrompts patches (or restores) every archive that contains a glyph atlas.
// mode: "xbox", "ps" or "kbm". Work is skipped when nothing changed since last time.
func applyButtonPrompts(gameData, mode string, binds map[string]string) error {
	mode = normalizePromptMode(mode)
	if promptsUpToDate(gameData, mode, binds) {
		return nil
	}
	sig := promptsSignature(mode, binds)
	archives, err := findGlyphArchives(gameData)
	if err != nil {
		return err
	}
	var done []string
	if mode == "xbox" {
		var firstErr error
		for _, a := range archives {
			if err := restoreArchive(a); err != nil && firstErr == nil {
				firstErr = err
			}
		}
		writePromptsState(gameData, promptsState{promptsVersion, sig, nil})
		return firstErr
	}

	// PS glyph source: the original hud_root atlas.
	var psAtlas *image.NRGBA
	if mode == "ps" {
		for _, a := range archives {
			if a.has[hudRootEntry] {
				payload, ents, _, err := readArchive(a.source())
				if err != nil {
					return err
				}
				e := ents[hudRootEntry]
				tex, err := decodeXenonTexture(payload[e.off : e.off+e.size])
				if err != nil {
					return err
				}
				psAtlas = tex.img
				break
			}
		}
		if psAtlas == nil {
			return errors.New("hud_root atlas not found")
		}
	}
	labels := keyLabels(binds)
	for _, a := range archives {
		if err := patchArchive(a, mode, psAtlas, labels); err != nil {
			return fmt.Errorf("%s: %w", filepath.Base(a.path), err)
		}
		done = append(done, a.path)
	}
	writePromptsState(gameData, promptsState{promptsVersion, sig, done})
	return nil
}

func writePromptsState(gameData string, st promptsState) {
	b, _ := json.MarshalIndent(st, "", "  ")
	_ = os.WriteFile(promptsStatePath(gameData), b, 0o644)
}

// ---------------------------------------------------------------------------
// Archives (FbRB: "FbRB", BE u32 tocSize, gzip TOC, gzip payload)
// ---------------------------------------------------------------------------

type glyphArchive struct {
	path string
	has  map[string]bool
}

func (a glyphArchive) source() string {
	if fileExists(a.path + ".orig") {
		return a.path + ".orig"
	}
	return a.path
}

type fbEntry struct{ off, size int }

func findGlyphArchives(gameData string) ([]glyphArchive, error) {
	want := map[string]bool{}
	for _, s := range atlasSpecs {
		want[s.Entry] = true
	}
	var out []glyphArchive
	dist := filepath.Join(gameData, "dist")
	if !dirExists(dist) {
		dist = filepath.Join(gameData, "Dist")
	}
	err := filepath.WalkDir(dist, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.EqualFold(filepath.Ext(p), ".fbrb") {
			return nil
		}
		a := glyphArchive{path: p, has: map[string]bool{}}
		names, err := readTOCNames(a.source())
		if err != nil {
			return nil
		}
		for _, n := range names {
			if want[n] {
				a.has[n] = true
			}
		}
		if len(a.has) > 0 {
			out = append(out, a)
		}
		return nil
	})
	return out, err
}

func parseTOC(toc []byte) (map[string]fbEntry, []string, error) {
	if len(toc) < 12 {
		return nil, nil, errors.New("short toc")
	}
	slen := int(binary.BigEndian.Uint32(toc[4:8]))
	if 8+slen+4 > len(toc) {
		return nil, nil, errors.New("bad toc")
	}
	strs := toc[8 : 8+slen]
	rest := toc[8+slen:]
	cnt := int(binary.BigEndian.Uint32(rest[:4]))
	if 4+cnt*24 > len(rest) {
		return nil, nil, errors.New("bad toc count")
	}
	str := func(o int) string {
		if o >= len(strs) {
			return ""
		}
		e := bytes.IndexByte(strs[o:], 0)
		if e < 0 {
			return string(strs[o:])
		}
		return string(strs[o : o+e])
	}
	ents := map[string]fbEntry{}
	var names []string
	for i := 0; i < cnt; i++ {
		b := rest[4+i*24:]
		name := strings.ToLower(str(int(binary.BigEndian.Uint32(b[0:4]))))
		ents[name] = fbEntry{int(binary.BigEndian.Uint32(b[8:12])), int(binary.BigEndian.Uint32(b[12:16]))}
		names = append(names, name)
	}
	return ents, names, nil
}

func gunzip(b []byte) ([]byte, error) {
	r, err := gzip.NewReader(bytes.NewReader(b))
	if err != nil {
		return nil, err
	}
	defer r.Close()
	return io.ReadAll(r)
}

func readTOCNames(path string) ([]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var hdr [8]byte
	if _, err := io.ReadFull(f, hdr[:]); err != nil {
		return nil, err
	}
	if string(hdr[:4]) != "FbRB" {
		return nil, errors.New("not fbrb")
	}
	n := binary.BigEndian.Uint32(hdr[4:])
	if n > 64<<20 {
		return nil, errors.New("toc too big")
	}
	buf := make([]byte, n)
	if _, err := io.ReadFull(f, buf); err != nil {
		return nil, err
	}
	toc, err := gunzip(buf)
	if err != nil {
		return nil, err
	}
	_, names, err := parseTOC(toc)
	return names, err
}

// readArchive returns the decompressed payload, entries and the raw header+TOC bytes.
func readArchive(path string) ([]byte, map[string]fbEntry, []byte, error) {
	d, err := os.ReadFile(path)
	if err != nil {
		return nil, nil, nil, err
	}
	if len(d) < 8 || string(d[:4]) != "FbRB" {
		return nil, nil, nil, errors.New("not fbrb")
	}
	n := int(binary.BigEndian.Uint32(d[4:8]))
	if 8+n > len(d) {
		return nil, nil, nil, errors.New("truncated")
	}
	toc, err := gunzip(d[8 : 8+n])
	if err != nil {
		return nil, nil, nil, err
	}
	ents, _, err := parseTOC(toc)
	if err != nil {
		return nil, nil, nil, err
	}
	payload, err := gunzip(d[8+n:])
	if err != nil {
		return nil, nil, nil, err
	}
	for _, e := range ents {
		if e.off+e.size > len(payload) {
			return nil, nil, nil, errors.New("entry out of range")
		}
	}
	return payload, ents, d[:8+n], nil
}

func restoreArchive(a glyphArchive) error {
	orig := a.path + ".orig"
	if !fileExists(orig) {
		return nil
	}
	if err := copyFile(orig, a.path); err != nil {
		return err
	}
	return os.Remove(orig)
}

func patchArchive(a glyphArchive, mode string, psAtlas *image.NRGBA, labels map[string]string) error {
	orig := a.path + ".orig"
	if !fileExists(orig) {
		if err := copyFile(a.path, orig); err != nil {
			return err
		}
	}
	payload, ents, head, err := readArchive(orig)
	if err != nil {
		return err
	}
	changed := false
	for _, spec := range atlasSpecs {
		e, ok := ents[spec.Entry]
		if !ok {
			continue
		}
		raw := payload[e.off : e.off+e.size]
		tex, err := decodeXenonTexture(raw)
		if err != nil {
			return err
		}
		if tex.w != spec.W || tex.h != spec.H || tex.fmt != spec.Fmt {
			continue // unexpected atlas (different game version) - leave untouched
		}
		var dirty []image.Rectangle
		for _, g := range spec.Glyphs {
			paintGlyph(tex.img, g, mode, psAtlas, labels)
			dirty = append(dirty, g.R.img())
		}
		if err := tex.encodeInto(raw, dirty); err != nil {
			return err
		}
		changed = true
	}
	if !changed {
		return nil
	}
	var buf bytes.Buffer
	buf.Write(head)
	zw, _ := gzip.NewWriterLevel(&buf, gzip.BestSpeed)
	if _, err := zw.Write(payload); err != nil {
		return err
	}
	if err := zw.Close(); err != nil {
		return err
	}
	tmp := a.path + ".tmp"
	if err := os.WriteFile(tmp, buf.Bytes(), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, a.path)
}

// ---------------------------------------------------------------------------
// Glyph painting
// ---------------------------------------------------------------------------

func clearRect(img *image.NRGBA, r image.Rectangle) {
	for y := r.Min.Y; y < r.Max.Y; y++ {
		for x := r.Min.X; x < r.Max.X; x++ {
			img.SetNRGBA(x, y, color.NRGBA{})
		}
	}
}

// fitInto scales src (keeping its aspect) centred into dst rect r.
func fitInto(dst *image.NRGBA, r image.Rectangle, src image.Image, sr image.Rectangle) {
	sw, sh := float64(sr.Dx()), float64(sr.Dy())
	s := math.Min(float64(r.Dx())/sw, float64(r.Dy())/sh)
	w, h := int(math.Round(sw*s)), int(math.Round(sh*s))
	x := r.Min.X + (r.Dx()-w)/2
	y := r.Min.Y + (r.Dy()-h)/2
	draw.CatmullRom.Scale(dst, image.Rect(x, y, x+w, y+h), src, sr, draw.Over, nil)
}

func paintGlyph(img *image.NRGBA, g glyph, mode string, ps *image.NRGBA, labels map[string]string) {
	r := g.R.img()
	switch {
	case g.Mono && mode == "ps":
		clearRect(img, r)
		drawPSMono(img, r, g.Btn)
	case g.Mono:
		clearRect(img, r)
		drawKeycap(img, r, labels[g.Btn], true)
	case mode == "ps":
		key := g.Btn
		if g.Alt {
			key = "B_ALT"
		}
		src, ok := psSource[key]
		if !ok {
			return
		}
		clearRect(img, r)
		fitInto(img, r, ps, src.img())
	default:
		clearRect(img, r)
		drawKeycap(img, r, labels[g.Btn], false)
	}
}

// Anti-aliased coverage helpers (supersampled 4x4).
func coverage(x, y int, inside func(fx, fy float64) bool) float64 {
	n := 0
	for sy := 0; sy < 4; sy++ {
		for sx := 0; sx < 4; sx++ {
			if inside(float64(x)+(float64(sx)+0.5)/4, float64(y)+(float64(sy)+0.5)/4) {
				n++
			}
		}
	}
	return float64(n) / 16
}

func roundRectInside(r image.Rectangle, rad float64) func(fx, fy float64) bool {
	x0, y0, x1, y1 := float64(r.Min.X), float64(r.Min.Y), float64(r.Max.X), float64(r.Max.Y)
	return func(fx, fy float64) bool {
		if fx < x0 || fy < y0 || fx > x1 || fy > y1 {
			return false
		}
		cx := math.Max(x0+rad, math.Min(fx, x1-rad))
		cy := math.Max(y0+rad, math.Min(fy, y1-rad))
		return (fx-cx)*(fx-cx)+(fy-cy)*(fy-cy) <= rad*rad
	}
}

func blend(img *image.NRGBA, x, y int, c color.NRGBA, a float64) {
	if a <= 0 {
		return
	}
	d := img.NRGBAAt(x, y)
	sa := float64(c.A) / 255 * a
	da := float64(d.A) / 255
	oa := sa + da*(1-sa)
	if oa <= 0 {
		img.SetNRGBA(x, y, color.NRGBA{})
		return
	}
	mix := func(s, dd uint8) uint8 {
		return uint8(math.Round((float64(s)*sa + float64(dd)*da*(1-sa)) / oa))
	}
	img.SetNRGBA(x, y, color.NRGBA{mix(c.R, d.R), mix(c.G, d.G), mix(c.B, d.B), uint8(math.Round(oa * 255))})
}

var boldFont *opentype.Font

func textMask(label string, maxW, maxH int) (*image.Alpha, error) {
	if boldFont == nil {
		f, err := opentype.Parse(gobold.TTF)
		if err != nil {
			return nil, err
		}
		boldFont = f
	}
	size := float64(maxH)
	for ; size >= 5; size -= 0.5 {
		face, err := opentype.NewFace(boldFont, &opentype.FaceOptions{Size: size, DPI: 72, Hinting: font.HintingNone})
		if err != nil {
			return nil, err
		}
		m := face.Metrics()
		adv := font.MeasureString(face, label).Ceil()
		capH := int(math.Ceil(float64(m.Ascent.Round()) * 0.72))
		if adv <= maxW && capH <= maxH {
			w := adv + 2
			h := maxH + 2
			mask := image.NewAlpha(image.Rect(0, 0, w, h))
			baseline := (h + capH) / 2
			d := font.Drawer{Dst: mask, Src: image.Opaque, Face: face, Dot: fixed.P(1, baseline)}
			d.DrawString(label)
			face.Close()
			return mask, nil
		}
		face.Close()
	}
	return image.NewAlpha(image.Rect(0, 0, 1, 1)), nil
}

// drawKeycap draws a keyboard key with the label. mono=true draws a white key with the
// text cut out (matches the white glyph style used inside text sprites).
func drawKeycap(img *image.NRGBA, r image.Rectangle, label string, mono bool) {
	if label == "" {
		label = "?"
	}
	w, h := r.Dx(), r.Dy()
	// Keys are drawn at most 1.6:1 wide inside tall/square slots so they read as keys.
	kw, kh := w, h
	if !mono && kh > 18 {
		kh = int(float64(h) * 0.78)
	}
	key := image.Rect(r.Min.X+(w-kw)/2, r.Min.Y+(h-kh)/2, r.Min.X+(w-kw)/2+kw, r.Min.Y+(h-kh)/2+kh)
	rad := math.Max(2, float64(kh)*0.18)
	border := math.Max(1, float64(kh)/14)
	outer := roundRectInside(key, rad)
	inner := roundRectInside(image.Rect(key.Min.X+int(math.Round(border)), key.Min.Y+int(math.Round(border)),
		key.Max.X-int(math.Round(border)), key.Max.Y-int(math.Round(border))), math.Max(1, rad-border))
	pad := int(math.Max(2, float64(kh)*0.16))
	if mono {
		pad = 2
	}
	tm, _ := textMask(label, kw-2*pad, int(float64(kh)*0.52))
	// Tiny keys: shorten the label until it fits (SPACE -> SP -> S).
	for r := []rune(label); tm.Bounds().Dx() <= 1 && len(r) > 1; {
		if len(r) > 2 {
			r = r[:2]
		} else {
			r = r[:1]
		}
		tm, _ = textMask(string(r), kw-2*pad, int(float64(kh)*0.62))
	}
	tx := key.Min.X + (kw-tm.Bounds().Dx())/2
	ty := key.Min.Y + (kh-tm.Bounds().Dy())/2
	textA := func(x, y int) float64 {
		p := image.Pt(x-tx, y-ty)
		if !p.In(tm.Bounds()) {
			return 0
		}
		return float64(tm.AlphaAt(p.X, p.Y).A) / 255
	}
	white := color.NRGBA{245, 245, 245, 255}
	dark := color.NRGBA{22, 22, 24, 235}
	for y := key.Min.Y; y < key.Max.Y; y++ {
		for x := key.Min.X; x < key.Max.X; x++ {
			co := coverage(x, y, outer)
			if co == 0 {
				continue
			}
			if mono {
				a := co * (1 - textA(x, y))
				blend(img, x, y, white, a)
				continue
			}
			ci := coverage(x, y, inner)
			blend(img, x, y, white, co)             // border
			blend(img, x, y, dark, ci)              // face
			blend(img, x, y, white, ci*textA(x, y)) // label
		}
	}
}

// drawPSMono draws a white disc with the PS symbol cut out (for "(A) SELECT" style sprites).
func drawPSMono(img *image.NRGBA, r image.Rectangle, btn string) {
	cx := float64(r.Min.X) + float64(r.Dx())/2
	cy := float64(r.Min.Y) + float64(r.Dy())/2
	R := math.Min(float64(r.Dx()), float64(r.Dy())) / 2
	sym := symbolInside(btn, cx, cy, R*0.55, math.Max(0.9, R*0.13))
	white := color.NRGBA{255, 255, 255, 255}
	for y := r.Min.Y; y < r.Max.Y; y++ {
		for x := r.Min.X; x < r.Max.X; x++ {
			a := coverage(x, y, func(fx, fy float64) bool {
				return (fx-cx)*(fx-cx)+(fy-cy)*(fy-cy) <= R*R && !sym(fx, fy)
			})
			blend(img, x, y, white, a)
		}
	}
}

func segDist(px, py, ax, ay, bx, by float64) float64 {
	vx, vy := bx-ax, by-ay
	t := ((px-ax)*vx + (py-ay)*vy) / (vx*vx + vy*vy)
	t = math.Max(0, math.Min(1, t))
	dx, dy := px-(ax+t*vx), py-(ay+t*vy)
	return math.Hypot(dx, dy)
}

// symbolInside returns a stroke test for ✕ ○ □ △ of radius s and stroke half-width t.
func symbolInside(btn string, cx, cy, s, t float64) func(fx, fy float64) bool {
	switch btn {
	case "A": // cross
		return func(fx, fy float64) bool {
			return segDist(fx, fy, cx-s, cy-s, cx+s, cy+s) <= t || segDist(fx, fy, cx-s, cy+s, cx+s, cy-s) <= t
		}
	case "B": // circle
		return func(fx, fy float64) bool { return math.Abs(math.Hypot(fx-cx, fy-cy)-s) <= t }
	case "X": // square
		return func(fx, fy float64) bool {
			q := s * 0.85
			return segDist(fx, fy, cx-q, cy-q, cx+q, cy-q) <= t || segDist(fx, fy, cx+q, cy-q, cx+q, cy+q) <= t ||
				segDist(fx, fy, cx+q, cy+q, cx-q, cy+q) <= t || segDist(fx, fy, cx-q, cy+q, cx-q, cy-q) <= t
		}
	default: // triangle
		ax, ay := cx, cy-s
		bx, by := cx+s*0.95, cy+s*0.65
		qx, qy := cx-s*0.95, cy+s*0.65
		return func(fx, fy float64) bool {
			return segDist(fx, fy, ax, ay, bx, by) <= t || segDist(fx, fy, bx, by, qx, qy) <= t || segDist(fx, fy, qx, qy, ax, ay) <= t
		}
	}
}
