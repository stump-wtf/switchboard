package switchboard

// Font-subset coverage for the vendored charm-web typefaces (JetBrains Mono + Space Mono,
// ADR-0018): UI copy uses the arrow glyphs U+2190 (←) and U+2192 (→) plus the TUI status glyphs
// (✓, ·) in buttons, stage labels, and body copy, so every vendored woff2 subset must map them —
// otherwise the browser falls back to an OS font mid-word. The test decodes each woff2 (brotli
// stream per the WOFF2 spec) and walks the cmap.
//
// The woff2 binaries are OFL-licensed but must be vendored from a machine with network access
// (static/fonts/README.md documents the exact files); until they land, each absent file SKIPS
// loudly instead of failing, so the retarget is enforced the moment the files exist.
// Governing: ADR-0018 (vendored fonts, no CDN), SPEC-0015 REQ "Typography And Vendored Fonts".

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"testing"

	"github.com/andybalholm/brotli"
)

var fontFiles = []string{
	"static/fonts/jetbrains-mono-400.woff2",
	"static/fonts/jetbrains-mono-500.woff2",
	"static/fonts/jetbrains-mono-700.woff2",
	"static/fonts/space-mono-400.woff2",
	"static/fonts/space-mono-700.woff2",
}

// requiredRunes are glyphs the UI renders in text set in these faces (arrows in stage labels and
// buttons, ✓ in done states, · separators) plus the core ASCII range as a canary against
// over-subsetting.
var requiredRunes = []rune{0x2190, 0x2192, 0x2713, 'A', 'z', '0', '·'}

func TestVendoredFontSubsetsCoverGlyphs(t *testing.T) {
	missing := 0
	for _, path := range fontFiles {
		raw, err := StaticFS.ReadFile(path)
		if err != nil {
			missing++
			t.Logf("SKIP %s: not vendored yet (%v) — see static/fonts/README.md", path, err)
			continue
		}
		cmap, err := woff2Cmap(raw)
		if err != nil {
			t.Errorf("%s: decode cmap: %v", path, err)
			continue
		}
		for _, r := range requiredRunes {
			if !cmap.covers(uint32(r)) {
				t.Errorf("%s: missing glyph for U+%04X %q", path, r, string(r))
			}
		}
	}
	switch {
	case missing == len(fontFiles):
		t.Skipf("SKIPPED LOUDLY: none of the %d charm-web woff2 files are vendored yet — the UI "+
			"falls back to the system ui-monospace stack until they land (static/fonts/README.md). "+
			"Vendor jetbrains-mono-{400,500,700}.woff2 and space-mono-{400,700}.woff2 to arm this test.",
			len(fontFiles))
	case missing > 0:
		// Partial vendoring is worse than none: some UI text renders the vendored face and the
		// rest falls back mid-page. All-or-nothing.
		t.Errorf("%d of %d charm-web woff2 files are vendored — vendor the full set (static/fonts/README.md)",
			len(fontFiles)-missing, len(fontFiles))
	}
}

// ---- minimal WOFF2 reader (enough to locate and parse the cmap table) ----

// knownTags is the WOFF2 known-table-tags array; a directory entry's low 6 flag bits index into
// it (63 means an explicit 4-byte tag follows). Order is normative per the WOFF2 spec §5.2.
var knownTags = []string{
	"cmap", "head", "hhea", "hmtx", "maxp", "name", "OS/2", "post",
	"cvt ", "fpgm", "glyf", "loca", "prep", "CFF ", "VORG", "EBDT",
	"EBLC", "gasp", "hdmx", "kern", "LTSH", "PCLT", "VDMX", "vhea",
	"vmtx", "BASE", "GDEF", "GPOS", "GSUB", "EBSC", "JSTF", "MATH",
	"CBDT", "CBLC", "COLR", "CPAL", "SVG ", "sbix", "acnt", "avar",
	"bdat", "bloc", "bsln", "cvar", "fdsc", "feat", "fmtx", "fvar",
	"gvar", "hsty", "just", "lcar", "mort", "morx", "opbd", "prop",
	"trak", "Zapf", "Silf", "Glat", "Gloc", "Feat", "Sill",
}

type cmapTable struct{ data []byte }

// woff2Cmap decompresses a WOFF2 font and returns its cmap table.
func woff2Cmap(raw []byte) (*cmapTable, error) {
	if len(raw) < 48 || string(raw[:4]) != "wOF2" {
		return nil, fmt.Errorf("not a woff2 file")
	}
	numTables := int(binary.BigEndian.Uint16(raw[12:14]))
	totalCompressed := int(binary.BigEndian.Uint32(raw[20:24]))

	// Table directory: per-entry flags, optional tag, origLength, optional transformLength.
	pos := 48
	type entry struct {
		tag    string
		length int // bytes this table occupies in the decompressed stream
	}
	entries := make([]entry, 0, numTables)
	for i := 0; i < numTables; i++ {
		if pos >= len(raw) {
			return nil, fmt.Errorf("truncated table directory")
		}
		flags := raw[pos]
		pos++
		tagIdx := int(flags & 0x3f)
		xform := int(flags >> 6)
		var tag string
		if tagIdx == 63 {
			if pos+4 > len(raw) {
				return nil, fmt.Errorf("truncated arbitrary tag")
			}
			tag = string(raw[pos : pos+4])
			pos += 4
		} else {
			tag = knownTags[tagIdx]
		}
		origLen, n, err := readUIntBase128(raw[pos:])
		if err != nil {
			return nil, fmt.Errorf("table %s origLength: %w", tag, err)
		}
		pos += n
		length := int(origLen)
		// transformLength is present iff a non-null transform applies: version 0 is the
		// (transforming) default for glyf/loca, null for every other table.
		transformed := false
		if tag == "glyf" || tag == "loca" {
			transformed = xform != 3
		} else {
			transformed = xform != 0
		}
		if transformed {
			tl, n, err := readUIntBase128(raw[pos:])
			if err != nil {
				return nil, fmt.Errorf("table %s transformLength: %w", tag, err)
			}
			pos += n
			length = int(tl)
		}
		entries = append(entries, entry{tag: tag, length: length})
	}

	if pos+totalCompressed > len(raw) {
		return nil, fmt.Errorf("compressed block overruns file")
	}
	decomp, err := io.ReadAll(brotli.NewReader(bytes.NewReader(raw[pos : pos+totalCompressed])))
	if err != nil {
		return nil, fmt.Errorf("brotli: %w", err)
	}

	off := 0
	for _, e := range entries {
		if off+e.length > len(decomp) {
			return nil, fmt.Errorf("table %s overruns decompressed stream", e.tag)
		}
		if e.tag == "cmap" {
			return &cmapTable{data: decomp[off : off+e.length]}, nil
		}
		off += e.length
	}
	return nil, fmt.Errorf("no cmap table")
}

// covers reports whether any format 4 or format 12 cmap subtable maps the codepoint to a
// non-zero glyph.
func (c *cmapTable) covers(cp uint32) bool {
	d := c.data
	if len(d) < 4 {
		return false
	}
	n := int(binary.BigEndian.Uint16(d[2:4]))
	for i := 0; i < n; i++ {
		rec := 4 + i*8
		if rec+8 > len(d) {
			return false
		}
		off := int(binary.BigEndian.Uint32(d[rec+4 : rec+8]))
		if off+2 > len(d) {
			continue
		}
		switch binary.BigEndian.Uint16(d[off : off+2]) {
		case 4:
			if format4Lookup(d[off:], cp) {
				return true
			}
		case 12:
			if format12Lookup(d[off:], cp) {
				return true
			}
		}
	}
	return false
}

func format4Lookup(st []byte, cp uint32) bool {
	if cp > 0xffff || len(st) < 14 {
		return false
	}
	c := uint16(cp)
	segX2 := int(binary.BigEndian.Uint16(st[6:8]))
	segs := segX2 / 2
	endBase, startBase := 14, 14+segX2+2
	deltaBase, rangeBase := startBase+segX2, startBase+2*segX2
	if rangeBase+segX2 > len(st) {
		return false
	}
	for i := 0; i < segs; i++ {
		end := binary.BigEndian.Uint16(st[endBase+2*i:])
		if end < c {
			continue
		}
		start := binary.BigEndian.Uint16(st[startBase+2*i:])
		if start > c {
			return false
		}
		delta := binary.BigEndian.Uint16(st[deltaBase+2*i:])
		ro := binary.BigEndian.Uint16(st[rangeBase+2*i:])
		if ro == 0 {
			return c+delta != 0
		}
		// glyphIdArray addressing is relative to this segment's idRangeOffset slot.
		addr := rangeBase + 2*i + int(ro) + 2*int(c-start)
		if addr+2 > len(st) {
			return false
		}
		g := binary.BigEndian.Uint16(st[addr:])
		return g != 0 && g+delta != 0
	}
	return false
}

func format12Lookup(st []byte, cp uint32) bool {
	if len(st) < 16 {
		return false
	}
	groups := int(binary.BigEndian.Uint32(st[12:16]))
	for i := 0; i < groups; i++ {
		g := 16 + i*12
		if g+12 > len(st) {
			return false
		}
		start := binary.BigEndian.Uint32(st[g:])
		end := binary.BigEndian.Uint32(st[g+4:])
		if cp >= start && cp <= end {
			return binary.BigEndian.Uint32(st[g+8:])+(cp-start) != 0
		}
	}
	return false
}

// readUIntBase128 decodes the WOFF2 variable-length integer (7 bits per byte, MSB first,
// high bit = continuation; max 5 bytes).
func readUIntBase128(b []byte) (uint32, int, error) {
	var v uint32
	for i := 0; i < 5 && i < len(b); i++ {
		if v&0xfe000000 != 0 {
			return 0, 0, fmt.Errorf("UIntBase128 overflow")
		}
		v = v<<7 | uint32(b[i]&0x7f)
		if b[i]&0x80 == 0 {
			return v, i + 1, nil
		}
	}
	return 0, 0, fmt.Errorf("UIntBase128 exceeds 5 bytes or input")
}
