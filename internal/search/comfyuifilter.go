package search

import (
	"cmp"
	"strconv"
	"strings"
	"unicode/utf8"
)

// ComfyTermRange is the [lo, hi) bound of a prefix scan over comfyui_terms.
func ComfyTermRange(prefix string) (string, string) {
	return prefix, prefix + prefixCeil
}

func (b *whereBuilder) buildComfyUIFilter(e FilterExpr) string {
	val := strings.TrimSpace(e.Val)
	// Off comfyui_metadata: finding distinct images in the terms means
	// walking every row.
	if val == "" {
		return "i.id IN (SELECT image_id FROM comfyui_metadata WHERE terms_version > 0)"
	}
	name, op, value := splitComfyTerm(val)
	if name == "" {
		return "1=0"
	}
	if comfyIsNumeric(op, value) {
		if strings.ContainsRune(name, '*') {
			return "1=0"
		}
		return b.comfyNumericTerm(name, op, value)
	}
	// No prefix to seek: the whole index is scanned.
	if strings.ContainsRune(name, '*') {
		b.args = append(b.args, globPattern(name)+"="+globPattern(cmp.Or(value, "*")))
		return comfyTermsIn(`term LIKE ? ESCAPE '\'`)
	}
	prefix := name + "="
	switch {
	case value == "":
		return b.comfyPrefixSeek(prefix, "")
	case !strings.ContainsRune(value, '*'):
		b.args = append(b.args, prefix+value)
		return comfyTermsIn("term = ? COLLATE NOCASE")
	}
	if stem, ok := strings.CutSuffix(value, "*"); ok && !strings.ContainsRune(stem, '*') {
		return b.comfyPrefixSeek(prefix+stem, "")
	}
	return b.comfyPrefixSeek(prefix, globPattern(prefix+value))
}

// Uncorrelated, so the term index is sought once instead of probed per
// visible image.
func comfyTermsIn(pred string) string {
	return "i.id IN (SELECT image_id FROM comfyui_terms WHERE " + pred + ")"
}

func (b *whereBuilder) comfyPrefixSeek(prefix, like string) string {
	b.args = append(b.args, prefix, prefix+prefixCeil)
	pred := "term >= ? COLLATE NOCASE AND term < ? COLLATE NOCASE"
	if like != "" {
		b.args = append(b.args, like)
		pred += ` AND term LIKE ? ESCAPE '\'`
	}
	return comfyTermsIn(pred)
}

func (b *whereBuilder) comfyNumericTerm(name, op, value string) string {
	comparison := value
	if op != "=" {
		comparison = op + value
	}
	// 1-based offset of the first character past `<name>=`, counted in
	// characters as substr counts them.
	off := strconv.Itoa(utf8.RuneCountInString(name) + 2)
	// A throwaway builder: a rejected value must not leave args behind
	// for a clause never emitted.
	sub := &whereBuilder{}
	clause := sub.buildCompFilter(`CAST(substr(term, `+off+`) AS REAL) %s ?`, comparison, parseFloatValue)
	if clause == "1=0" {
		return "1=0"
	}
	prefix := name + "="
	b.args = append(b.args, prefix, prefix+prefixCeil)
	b.args = append(b.args, sub.args...)
	// CAST reads a non-numeric value as 0.0, so without the GLOB every
	// word in the slice would answer `<=`.
	return comfyTermsIn(`term >= ? COLLATE NOCASE AND term < ? COLLATE NOCASE` +
		` AND substr(term, ` + off + `) GLOB '[-0-9]*' AND ` + clause)
}

func splitComfyTerm(val string) (name, op, value string) {
	i := strings.IndexAny(val, "=<>")
	if i < 0 {
		return "node", "=", val
	}
	op, value = val[i:i+1], val[i+1:]
	if op != "=" && strings.HasPrefix(value, "=") {
		op, value = op+"=", value[1:]
	}
	return val[:i], op, value
}

// `=` counts as a comparison only for a range whose halves parse: a
// checkpoint name may carry `..`.
func comfyIsNumeric(op, value string) bool {
	if op != "=" {
		return true
	}
	lo, hi, ok := strings.Cut(value, "..")
	if !ok || (lo == "" && hi == "") {
		return false
	}
	return comfyFloatHalf(lo) && comfyFloatHalf(hi)
}

func comfyFloatHalf(s string) bool {
	s = strings.TrimSpace(s)
	if s == "" {
		return true
	}
	_, err := strconv.ParseFloat(s, 64)
	return err == nil
}
