// Xbox 360 XenonTexture (.res) decode / partial re-encode.
// Header: 92 bytes (BE): fmt @8, width @16, height @20. Data is tiled
// (XGAddress2DTiledOffset). DXT blocks are stored with 16-bit byte swapping.
// Formats used by the UI atlases: 0 = DXT1, 2 = DXT5, 9 = A8R8G8B8.
package main

import (
	"encoding/binary"
	"errors"
	"fmt"
	"image"
	"image/color"
	"math"
)

const xtexHeader = 92

type xenonTexture struct {
	fmt  uint32
	w, h int
	img  *image.NRGBA
}

func tiledOffset(x, y, width, logBpp int) int {
	alignedWidth := (width + 31) &^ 31
	macro := ((x >> 5) + (y>>5)*(alignedWidth>>5)) << (logBpp + 7)
	micro := ((x & 7) + ((y & 6) << 2)) << logBpp
	offset := macro + ((micro &^ 15) << 1) + (micro & 15) + ((y & 8) << (3 + logBpp)) + ((y & 1) << 4)
	return (((offset &^ 511) << 3) + ((offset & 448) << 2) + (offset & 63) + ((y & 16) << 7) +
		(((((y & 8) >> 2) + (x >> 3)) & 3) << 6)) >> logBpp
}

func texLayout(f uint32, w, h int) (blockW, blockH, bpp, logBpp int, err error) {
	switch f {
	case 0:
		return w / 4, h / 4, 8, 3, nil
	case 2:
		return w / 4, h / 4, 16, 4, nil
	case 9:
		return w, h, 4, 2, nil
	}
	return 0, 0, 0, 0, fmt.Errorf("unsupported texture format %d", f)
}

func decodeXenonTexture(b []byte) (*xenonTexture, error) {
	if len(b) < xtexHeader {
		return nil, errors.New("short texture")
	}
	t := &xenonTexture{
		fmt: binary.BigEndian.Uint32(b[8:12]),
		w:   int(binary.BigEndian.Uint32(b[16:20])),
		h:   int(binary.BigEndian.Uint32(b[20:24])),
	}
	bw, bh, bpp, lb, err := texLayout(t.fmt, t.w, t.h)
	if err != nil {
		return nil, err
	}
	d := b[xtexHeader:]
	t.img = image.NewNRGBA(image.Rect(0, 0, t.w, t.h))
	blk := make([]byte, bpp)
	for by := 0; by < bh; by++ {
		for bx := 0; bx < bw; bx++ {
			o := tiledOffset(bx, by, bw, lb) * bpp
			if o+bpp > len(d) {
				continue
			}
			copy(blk, d[o:o+bpp])
			switch t.fmt {
			case 9:
				t.img.SetNRGBA(bx, by, color.NRGBA{blk[1], blk[2], blk[3], blk[0]})
			default:
				swap16(blk)
				var px [16]color.NRGBA
				if t.fmt == 0 {
					decodeDXTColor(blk, &px, true)
				} else {
					decodeDXTColor(blk[8:], &px, false)
					decodeDXT5Alpha(blk[:8], &px)
				}
				for i := 0; i < 16; i++ {
					t.img.SetNRGBA(bx*4+i%4, by*4+i/4, px[i])
				}
			}
		}
	}
	return t, nil
}

// encodeInto re-encodes the texels/blocks touching the dirty rects back into raw (in place).
func (t *xenonTexture) encodeInto(raw []byte, dirty []image.Rectangle) error {
	bw, _, bpp, lb, err := texLayout(t.fmt, t.w, t.h)
	if err != nil {
		return err
	}
	d := raw[xtexHeader:]
	unit := 1
	if t.fmt != 9 {
		unit = 4
	}
	seen := map[int]bool{}
	for _, r := range dirty {
		r = r.Intersect(t.img.Rect)
		for by := r.Min.Y / unit; by <= (r.Max.Y-1)/unit; by++ {
			for bx := r.Min.X / unit; bx <= (r.Max.X-1)/unit; bx++ {
				o := tiledOffset(bx, by, bw, lb) * bpp
				if seen[o] || o+bpp > len(d) {
					continue
				}
				seen[o] = true
				if t.fmt == 9 {
					c := t.img.NRGBAAt(bx, by)
					d[o], d[o+1], d[o+2], d[o+3] = c.A, c.R, c.G, c.B
					continue
				}
				var px [16]color.NRGBA
				for i := 0; i < 16; i++ {
					px[i] = t.img.NRGBAAt(bx*4+i%4, by*4+i/4)
				}
				blk := d[o : o+bpp]
				if t.fmt == 0 {
					encodeDXTColor(px, blk, true)
				} else {
					encodeDXT5Alpha(px, blk[:8])
					encodeDXTColor(px, blk[8:], false)
				}
				swap16(blk)
			}
		}
	}
	return nil
}

func swap16(b []byte) {
	for i := 0; i+1 < len(b); i += 2 {
		b[i], b[i+1] = b[i+1], b[i]
	}
}

func rgb565(c uint16) (uint8, uint8, uint8) {
	r := uint8((c >> 11) & 31)
	g := uint8((c >> 5) & 63)
	b := uint8(c & 31)
	return r<<3 | r>>2, g<<2 | g>>4, b<<3 | b>>2
}

func decodeDXTColor(b []byte, px *[16]color.NRGBA, dxt1 bool) {
	c0 := binary.LittleEndian.Uint16(b[0:2])
	c1 := binary.LittleEndian.Uint16(b[2:4])
	var pal [4]color.NRGBA
	r0, g0, b0 := rgb565(c0)
	r1, g1, b1 := rgb565(c1)
	pal[0] = color.NRGBA{r0, g0, b0, 255}
	pal[1] = color.NRGBA{r1, g1, b1, 255}
	if c0 > c1 || !dxt1 {
		pal[2] = color.NRGBA{uint8((2*int(r0) + int(r1)) / 3), uint8((2*int(g0) + int(g1)) / 3), uint8((2*int(b0) + int(b1)) / 3), 255}
		pal[3] = color.NRGBA{uint8((int(r0) + 2*int(r1)) / 3), uint8((int(g0) + 2*int(g1)) / 3), uint8((int(b0) + 2*int(b1)) / 3), 255}
	} else {
		pal[2] = color.NRGBA{uint8((int(r0) + int(r1)) / 2), uint8((int(g0) + int(g1)) / 2), uint8((int(b0) + int(b1)) / 2), 255}
		pal[3] = color.NRGBA{}
	}
	idx := binary.LittleEndian.Uint32(b[4:8])
	for i := 0; i < 16; i++ {
		px[i] = pal[(idx>>(2*i))&3]
	}
}

func decodeDXT5Alpha(b []byte, px *[16]color.NRGBA) {
	a0, a1 := int(b[0]), int(b[1])
	var pal [8]uint8
	pal[0], pal[1] = uint8(a0), uint8(a1)
	if a0 > a1 {
		for i := 1; i < 7; i++ {
			pal[i+1] = uint8(((7-i)*a0 + i*a1) / 7)
		}
	} else {
		for i := 1; i < 5; i++ {
			pal[i+1] = uint8(((5-i)*a0 + i*a1) / 5)
		}
		pal[6], pal[7] = 0, 255
	}
	var bits uint64
	for i := 0; i < 6; i++ {
		bits |= uint64(b[2+i]) << (8 * i)
	}
	for i := 0; i < 16; i++ {
		px[i].A = pal[(bits>>(3*i))&7]
	}
}

func to565(r, g, b float64) uint16 {
	cl := func(v float64, m float64) uint16 { return uint16(math.Max(0, math.Min(m, math.Round(v*m/255)))) }
	return cl(r, 31)<<11 | cl(g, 63)<<5 | cl(b, 31)
}

// encodeDXTColor: bounding-box endpoints along the principal axis, nearest index.
func encodeDXTColor(px [16]color.NRGBA, out []byte, dxt1 bool) {
	// Only opaque-ish texels decide the colour endpoints.
	var pts [][3]float64
	for _, p := range px {
		if p.A >= 8 || !dxt1 {
			if !dxt1 && p.A < 4 {
				continue
			}
			pts = append(pts, [3]float64{float64(p.R), float64(p.G), float64(p.B)})
		}
	}
	if len(pts) == 0 {
		for i := range out[:8] {
			out[i] = 0
		}
		if dxt1 {
			// c0 <= c1 with all indices 3 = transparent
			binary.LittleEndian.PutUint32(out[4:8], 0xFFFFFFFF)
		}
		return
	}
	var mean [3]float64
	for _, p := range pts {
		for k := 0; k < 3; k++ {
			mean[k] += p[k] / float64(len(pts))
		}
	}
	// principal axis by power iteration on covariance
	var cov [3][3]float64
	for _, p := range pts {
		for i := 0; i < 3; i++ {
			for j := 0; j < 3; j++ {
				cov[i][j] += (p[i] - mean[i]) * (p[j] - mean[j])
			}
		}
	}
	axis := [3]float64{1, 1, 1}
	for it := 0; it < 8; it++ {
		var n [3]float64
		for i := 0; i < 3; i++ {
			n[i] = cov[i][0]*axis[0] + cov[i][1]*axis[1] + cov[i][2]*axis[2]
		}
		l := math.Sqrt(n[0]*n[0] + n[1]*n[1] + n[2]*n[2])
		if l < 1e-9 {
			break
		}
		axis = [3]float64{n[0] / l, n[1] / l, n[2] / l}
	}
	lo, hi := math.Inf(1), math.Inf(-1)
	for _, p := range pts {
		t := (p[0]-mean[0])*axis[0] + (p[1]-mean[1])*axis[1] + (p[2]-mean[2])*axis[2]
		lo, hi = math.Min(lo, t), math.Max(hi, t)
	}
	e0 := [3]float64{mean[0] + axis[0]*hi, mean[1] + axis[1]*hi, mean[2] + axis[2]*hi}
	e1 := [3]float64{mean[0] + axis[0]*lo, mean[1] + axis[1]*lo, mean[2] + axis[2]*lo}
	c0, c1 := to565(e0[0], e0[1], e0[2]), to565(e1[0], e1[1], e1[2])
	hasTransparent := false
	if dxt1 {
		for _, p := range px {
			if p.A < 128 {
				hasTransparent = true
			}
		}
	}
	if hasTransparent {
		if c0 > c1 {
			c0, c1 = c1, c0
		}
	} else {
		if c0 < c1 {
			c0, c1 = c1, c0
		}
	}
	binary.LittleEndian.PutUint16(out[0:2], c0)
	binary.LittleEndian.PutUint16(out[2:4], c1)
	var pal [4][3]float64
	r0, g0, b0 := rgb565(c0)
	r1, g1, b1 := rgb565(c1)
	pal[0] = [3]float64{float64(r0), float64(g0), float64(b0)}
	pal[1] = [3]float64{float64(r1), float64(g1), float64(b1)}
	n := 4
	if hasTransparent || (dxt1 && c0 <= c1) {
		pal[2] = [3]float64{(pal[0][0] + pal[1][0]) / 2, (pal[0][1] + pal[1][1]) / 2, (pal[0][2] + pal[1][2]) / 2}
		n = 3
	} else {
		for k := 0; k < 3; k++ {
			pal[2][k] = (2*pal[0][k] + pal[1][k]) / 3
			pal[3][k] = (pal[0][k] + 2*pal[1][k]) / 3
		}
	}
	var idx uint32
	for i, p := range px {
		if hasTransparent && p.A < 128 {
			idx |= 3 << (2 * i)
			continue
		}
		best, bd := 0, math.Inf(1)
		for k := 0; k < n; k++ {
			dr, dg, db := float64(p.R)-pal[k][0], float64(p.G)-pal[k][1], float64(p.B)-pal[k][2]
			if dd := dr*dr + dg*dg + db*db; dd < bd {
				best, bd = k, dd
			}
		}
		idx |= uint32(best) << (2 * i)
	}
	binary.LittleEndian.PutUint32(out[4:8], idx)
}

func encodeDXT5Alpha(px [16]color.NRGBA, out []byte) {
	lo, hi := 255, 0
	for _, p := range px {
		a := int(p.A)
		if a < lo {
			lo = a
		}
		if a > hi {
			hi = a
		}
	}
	var pal [8]int
	var a0, a1 int
	if lo == hi {
		a0, a1 = hi, lo
	} else {
		a0, a1 = hi, lo // a0 > a1: 8-value interpolation
	}
	pal[0], pal[1] = a0, a1
	if a0 > a1 {
		for i := 1; i < 7; i++ {
			pal[i+1] = ((7-i)*a0 + i*a1) / 7
		}
	} else {
		for i := 1; i < 5; i++ {
			pal[i+1] = ((5-i)*a0 + i*a1) / 5
		}
		pal[6], pal[7] = 0, 255
	}
	out[0], out[1] = uint8(a0), uint8(a1)
	var bits uint64
	for i, p := range px {
		best, bd := 0, 1<<30
		for k := 0; k < 8; k++ {
			d := int(p.A) - pal[k]
			if d < 0 {
				d = -d
			}
			if d < bd {
				best, bd = k, d
			}
		}
		bits |= uint64(best) << (3 * i)
	}
	for i := 0; i < 6; i++ {
		out[2+i] = uint8(bits >> (8 * i))
	}
}
