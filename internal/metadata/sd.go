package metadata

import (
	"encoding/json"
	"math"
	"strconv"
	"strings"

	"github.com/monbooru/monbooru/internal/models"
)

// An infinite value would make every JSON export of the row fail.
func parseFinite(s string) (float64, bool) {
	f, err := strconv.ParseFloat(s, 64)
	return f, err == nil && !math.IsInf(f, 0) && !math.IsNaN(f)
}

func sdFromEXIF(x *exifData) *models.SDMetadata {
	if x == nil {
		return nil
	}
	tag, ok := x.tags[userCommentField]
	if !ok {
		return nil
	}
	text, ok := tag.userCommentText()
	if !ok {
		return nil
	}
	return parseA1111Parameters(text)
}

// The "Negative prompt:" and "Steps:" markers tell A1111 text from what
// other tools write into UserComment.
func parseA1111Parameters(text string) *models.SDMetadata {
	text = strings.TrimSpace(text)
	if text == "" {
		return nil
	}
	// An empty prompt leaves the marker first, with no newline of its own.
	text = "\n" + text

	negIdx := strings.Index(text, "\nNegative prompt:")
	paramIdx := findParamLineIndex(text)
	if negIdx < 0 && paramIdx < 0 {
		return nil
	}

	sd := &models.SDMetadata{}
	var rawParams string
	if negIdx >= 0 {
		sd.Prompt = strings.TrimSpace(text[:negIdx])
		afterNeg := text[negIdx+len("\nNegative prompt:"):]
		if paramIdx > negIdx {
			relIdx := paramIdx - negIdx - len("\nNegative prompt:")
			if relIdx > 0 && relIdx <= len(afterNeg) {
				sd.NegativePrompt = strings.TrimSpace(afterNeg[:relIdx])
			}
			rawParams = strings.TrimSpace(text[paramIdx:])
			parseA1111Params(rawParams, sd)
		} else {
			sd.NegativePrompt = strings.TrimSpace(afterNeg)
		}
	} else {
		sd.Prompt = strings.TrimSpace(text[:paramIdx])
		rawParams = strings.TrimSpace(text[paramIdx:])
		parseA1111Params(rawParams, sd)
	}

	sd.RawParams = rawParams
	if rawParams != "" {
		sd.ParsedParams = ParseAllSDParams(rawParams)
	}
	sd.GenerationHash = computeGenerationHash(
		sd.Prompt, sd.NegativePrompt, sd.Model, sd.Sampler, sd.Steps, sd.CFGScale,
	)
	return sd
}

func findParamLineIndex(text string) int {
	lines := strings.Split(text, "\n")
	pos := 0
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "Steps:") {
			return pos
		}
		pos += len(line) + 1
	}
	return -1
}

// ParseAllSDParams keeps the line's order, and a braced or quoted value stays whole.
func ParseAllSDParams(paramLine string) []models.SDParam {
	var result []models.SDParam
	seen := map[string]bool{}
	eachA1111Param(paramLine, func(key, val string) {
		if !seen[key] {
			seen[key] = true
			result = append(result, models.SDParam{Key: key, Val: val})
		}
	})
	return result
}

// A1111 quotes a value holding a comma as a JSON string, and reads a quote
// as one only at the start of a value.
func splitA1111Params(s string) []string {
	var parts []string
	depth := 0
	start := 0
	quoted, escaped := false, false
	var prev rune
	for i, ch := range s {
		if quoted {
			switch {
			case escaped:
				escaped = false
			case ch == '\\':
				escaped = true
			case ch == '"':
				quoted = false
			}
			continue
		}
		switch ch {
		case '"':
			quoted = prev == ':'
		case '{', '(', '[':
			depth++
		case '}', ')', ']':
			if depth > 0 {
				depth--
			}
		case ',':
			if depth == 0 {
				parts = append(parts, s[start:i])
				start = i + 1
			}
		}
		if ch != ' ' {
			prev = ch
		}
	}
	if start < len(s) {
		parts = append(parts, s[start:])
	}
	return parts
}

// ParseSeed stores a seed past MaxInt64 as its two's-complement int64, so
// the whole uint64 range fits the INTEGER column.
func ParseSeed(s string) (int64, bool) {
	if n, err := strconv.ParseInt(s, 10, 64); err == nil {
		return n, true
	}
	if n, err := strconv.ParseUint(s, 10, 64); err == nil {
		return int64(n), true
	}
	return 0, false
}

func eachA1111Param(paramLine string, fn func(key, val string)) {
	paramLine = strings.ReplaceAll(paramLine, "\n", " ")
	for _, part := range splitA1111Params(paramLine) {
		key, val, ok := strings.Cut(strings.TrimSpace(part), ":")
		key, val = strings.TrimSpace(key), strings.TrimSpace(val)
		if !ok || key == "" {
			continue
		}
		if len(val) >= 2 && val[0] == '"' && val[len(val)-1] == '"' {
			var unquoted string
			if json.Unmarshal([]byte(val), &unquoted) == nil {
				val = unquoted
			}
		}
		fn(key, val)
	}
}

func parseA1111Params(paramLine string, sd *models.SDMetadata) {
	eachA1111Param(paramLine, func(key, val string) {
		switch key {
		case "Steps":
			if n, err := strconv.Atoi(val); err == nil {
				sd.Steps = &n
			}
		case "Sampler":
			sd.Sampler = val
		case "CFG scale":
			if f, ok := parseFinite(val); ok {
				sd.CFGScale = &f
			}
		case "Seed":
			if n, ok := ParseSeed(val); ok {
				sd.Seed = &n
			}
		case "Model":
			if sd.Model == "" {
				sd.Model = val
			} else {
				sd.Model += ", " + val
			}
		case "Lora hashes":
			loraVal := strings.TrimSpace(val)
			if loraVal != "" && loraVal != "{}" {
				if sd.Model != "" {
					sd.Model += " | LoRA: " + loraVal
				} else {
					sd.Model = "LoRA: " + loraVal
				}
			}
		}
	})
}
