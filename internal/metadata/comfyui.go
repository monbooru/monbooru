package metadata

import (
	"cmp"
	_ "embed"
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"

	"github.com/monbooru/monbooru/internal/models"
	"github.com/monbooru/monbooru/internal/shared/logx"
)

// Python's json writes NaN and Infinity, which encoding/json refuses;
// they become null outside strings.
func sanitizeComfyJSON(s string) string {
	if !strings.Contains(s, "NaN") && !strings.Contains(s, "Infinity") {
		return s
	}

	var b strings.Builder
	b.Grow(len(s))
	inString := false
	escape := false
	for i := 0; i < len(s); i++ {
		c := s[i]
		if inString {
			b.WriteByte(c)
			if escape {
				escape = false
				continue
			}
			switch c {
			case '\\':
				escape = true
			case '"':
				inString = false
			}
			continue
		}
		if c == '"' {
			inString = true
			b.WriteByte(c)
			continue
		}
		switch {
		case strings.HasPrefix(s[i:], "-Infinity"):
			b.WriteString("null")
			i += len("-Infinity") - 1
		case strings.HasPrefix(s[i:], "Infinity"):
			b.WriteString("null")
			i += len("Infinity") - 1
		case strings.HasPrefix(s[i:], "NaN"):
			b.WriteString("null")
			i += len("NaN") - 1
		default:
			b.WriteByte(c)
		}
	}
	return b.String()
}

func parseComfyPromptChunk(raw string) *models.ComfyUIMetadata {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	raw = sanitizeComfyJSON(raw)

	// API format: {"1": {"inputs": {...}, "class_type": "...", "_meta": {...}}, ...}
	var apiNodes map[string]json.RawMessage
	if err := json.Unmarshal([]byte(raw), &apiNodes); err != nil {
		return nil
	}

	parsedNodes := make(map[string]comfyAPINode, len(apiNodes))
	for id, nodeRaw := range apiNodes {
		var node comfyAPINode
		if err := json.Unmarshal(nodeRaw, &node); err == nil {
			parsedNodes[id] = node
		}
	}

	meta := &models.ComfyUIMetadata{}
	nodeIDs := comfyNodeIDs(parsedNodes)

	table := comfyNodeTable()

	// The first sampler's positive input says which text node is the prompt.
	positiveNodeID := ""
	for _, id := range nodeIDs {
		node := parsedNodes[id]
		if roles := table[node.typeName()]; roles.Positive != "" {
			if nid, ok := refNodeID(node.Inputs[roles.Positive]); ok {
				positiveNodeID = nid
			}
			break
		}
	}

	for _, id := range nodeIDs {
		node := parsedNodes[id]
		roles := table[node.typeName()]
		if textRaw, ok := node.Inputs[roles.Prompt]; ok && roles.Prompt != "" {
			text := resolveStringInput(textRaw, parsedNodes)
			if text != "" {
				if positiveNodeID != "" {
					if id == positiveNodeID {
						meta.Prompt = text
					}
				} else if meta.Prompt == "" {
					meta.Prompt = text
				}
			}
		}
		applyComfyNodeInputs(roles, node.Inputs, meta, parsedNodes)
	}

	if meta.Prompt == "" {
		for _, id := range nodeIDs {
			node := parsedNodes[id]
			if node.typeName() != "PrimitiveStringMultiline" {
				continue
			}
			if val, ok := nodeInput[string](node.Inputs, "value"); ok && len(val) > 10 {
				meta.Prompt = val
				break
			}
		}
	}

	// A graph the extractors all pass over is still ComfyUI: its nodes
	// stay searchable and only the recipe columns are empty.
	if !slices.ContainsFunc(nodeIDs, func(id string) bool { return parsedNodes[id].typeName() != "" }) {
		return nil
	}
	meta.RawWorkflow = raw
	setComfyHash(meta)
	return meta
}

// ReparseComfyWorkflow reads the recipe columns back out of a stored
// raw_workflow, which holds whichever of the two shapes ingest parsed.
func ReparseComfyWorkflow(raw string) *models.ComfyUIMetadata {
	if m := parseComfyPromptChunk(raw); m != nil {
		return m
	}
	return parseComfyWorkflow(raw)
}

// ParseComfyTags reads a video's prompt and workflow container tags with
// the precedence of the PNG chunks of the same names.
func ParseComfyTags(prompt, workflow string) *models.ComfyUIMetadata {
	if m := parseComfyPromptChunk(prompt); m != nil {
		return m
	}
	return parseComfyWorkflow(workflow)
}

// ComfyUI's WebP savers write the prompt and workflow into these EXIF fields.
var comfyEXIFFields = map[string]string{"Model": "prompt:", "Make": "workflow:"}

func comfyEXIFText(x *exifData, name string) string {
	prefix, ok := comfyEXIFFields[name]
	if !ok {
		return ""
	}
	tag, ok := x.tags[name]
	if !ok {
		return ""
	}
	s, _ := tag.stringVal()
	text, found := strings.CutPrefix(s, prefix)
	if !found {
		return ""
	}
	return text
}

func comfyFromEXIF(x *exifData) *models.ComfyUIMetadata {
	if x == nil {
		return nil
	}
	return ParseComfyTags(comfyEXIFText(x, "Model"), comfyEXIFText(x, "Make"))
}

type comfyGraphNode struct {
	Key       string
	ClassType string
	Title     string
	Inputs    map[string]json.RawMessage
}

// The node-graph shape lists nodes under "nodes"; the API shape keys them
// by id at the top level, where links, extra and version have no class
// and drop out.
func comfyGraphNodes(raw string) []comfyGraphNode {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	var top map[string]json.RawMessage
	if err := json.Unmarshal([]byte(sanitizeComfyJSON(raw)), &top); err != nil {
		return nil
	}
	if nodesRaw, ok := top["nodes"]; ok {
		var entries []json.RawMessage
		if err := json.Unmarshal(nodesRaw, &entries); err == nil {
			nodes := make([]comfyGraphNode, 0, len(entries))
			for _, entry := range entries {
				if node, ok := decodeComfyGraphNode(entry); ok && node.ClassType != "" {
					nodes = append(nodes, node)
				}
			}
			return nodes
		}
	}
	nodes := make([]comfyGraphNode, 0, len(top))
	for _, key := range comfyNodeIDs(top) {
		node, ok := decodeComfyGraphNode(top[key])
		if !ok || node.ClassType == "" {
			continue
		}
		node.Key = key
		nodes = append(nodes, node)
	}
	return nodes
}

func decodeComfyGraphNode(raw json.RawMessage) (comfyGraphNode, bool) {
	var n struct {
		ID        json.RawMessage `json:"id"`
		ClassType string          `json:"class_type"`
		Type      string          `json:"type"`
		Title     string          `json:"title"`
		Meta      struct {
			Title string `json:"title"`
		} `json:"_meta"`
		Inputs json.RawMessage `json:"inputs"`
	}
	if err := json.Unmarshal(raw, &n); err != nil {
		return comfyGraphNode{}, false
	}
	node := comfyGraphNode{
		Key:       strings.Trim(string(n.ID), `"`),
		ClassType: cmp.Or(n.ClassType, n.Type),
		Title:     cmp.Or(n.Meta.Title, n.Title),
	}
	// Node-graph inputs are link descriptors with no values: they read as
	// none rather than failing the node.
	_ = json.Unmarshal(n.Inputs, &node.Inputs)
	return node, true
}

// Node-graph values sit in a positional widgets_values array that means
// nothing without the node definitions, so the recipe is read only where
// inputs are named.
func parseComfyWorkflow(raw string) *models.ComfyUIMetadata {
	nodes := comfyGraphNodes(raw)
	if len(nodes) == 0 {
		return nil
	}
	meta := &models.ComfyUIMetadata{RawWorkflow: sanitizeComfyJSON(strings.TrimSpace(raw))}
	table := comfyNodeTable()
	for _, node := range nodes {
		roles := table[node.ClassType]
		if textRaw, ok := node.Inputs[roles.Prompt]; ok && roles.Prompt != "" && meta.Prompt == "" {
			meta.Prompt = resolveStringInput(textRaw, nil)
		}
		applyComfyNodeInputs(roles, node.Inputs, meta, nil)
	}
	setComfyHash(meta)
	return meta
}

func setComfyHash(meta *models.ComfyUIMetadata) {
	if meta.Prompt != "" || meta.ModelCheckpoint != "" || meta.Seed != nil {
		meta.GenerationHash = computeGenerationHash(
			meta.Prompt, "", meta.ModelCheckpoint, meta.Sampler, meta.Steps, meta.CFGScale,
		)
	}
}

type comfyAPINode struct {
	ClassType string                     `json:"class_type"`
	Type      string                     `json:"type"`
	Inputs    map[string]json.RawMessage `json:"inputs"`
}

func (n comfyAPINode) typeName() string { return cmp.Or(n.ClassType, n.Type) }

// A reference input is a [nodeID, slotIndex] array.
func refNodeID(raw json.RawMessage) (string, bool) {
	var ref []json.RawMessage
	if err := json.Unmarshal(raw, &ref); err != nil || len(ref) < 1 {
		return "", false
	}
	var nodeID string
	if err := json.Unmarshal(ref[0], &nodeID); err != nil {
		return "", false
	}
	return nodeID, true
}

func resolveRefInput[T any](raw json.RawMessage, nodes map[string]comfyAPINode, candidates []string, keep func(T) bool) (T, bool) {
	var zero, v T
	if err := json.Unmarshal(raw, &v); err == nil {
		return v, true
	}
	nodeID, ok := refNodeID(raw)
	if !ok {
		return zero, false
	}
	refNode, ok := nodes[nodeID]
	if !ok {
		return zero, false
	}
	for _, key := range candidates {
		if valRaw, ok := refNode.Inputs[key]; ok {
			var val T
			if err := json.Unmarshal(valRaw, &val); err == nil && keep(val) {
				return val, true
			}
		}
	}
	return zero, false
}

func resolveStringInput(raw json.RawMessage, nodes map[string]comfyAPINode) string {
	s, _ := resolveRefInput(raw, nodes, []string{"value", "text", "string"}, func(v string) bool { return v != "" })
	return s
}

// nodes may be nil, which takes a literal only. A seed past MaxInt64 is
// kept as its two's-complement int64.
func resolveSeedInput(raw json.RawMessage, nodes map[string]comfyAPINode) (int64, bool) {
	candidates := []string{"seed", "value", "int"}
	if n, ok := resolveRefInput(raw, nodes, candidates, func(int64) bool { return true }); ok {
		return n, true
	}
	n, ok := resolveRefInput(raw, nodes, candidates, func(uint64) bool { return true })
	return int64(n), ok
}

// ParseComfyWorkflowNodes keeps nodes with no inputs so the graph stays
// complete: a node-graph workflow has none to show.
func ParseComfyWorkflowNodes(raw string) []models.ComfyNode {
	graph := comfyGraphNodes(raw)
	nodes := make([]models.ComfyNode, 0, len(graph))
	for _, n := range graph {
		var params []models.ComfyNodeParam
		for _, pk := range slices.Sorted(maps.Keys(n.Inputs)) {
			if param := comfyParamToDisplay(pk, n.Inputs[pk]); param != nil {
				params = append(params, *param)
			}
		}

		nodes = append(nodes, models.ComfyNode{
			Key:        n.Key,
			Title:      cmp.Or(n.Title, n.ClassType),
			ClassType:  n.ClassType,
			Params:     params,
			SearchTerm: comfyTerm("node", n.ClassType),
		})
	}
	return nodes
}

// The display and the term index share this, so a shown value and its
// term cannot drift.
func comfyScalar(raw json.RawMessage) (string, bool) {
	if string(raw) == "null" {
		return "", false
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s, true
	}
	// float64 rounds an integer past 2^53.
	if n, err := strconv.ParseInt(string(raw), 10, 64); err == nil {
		return strconv.FormatInt(n, 10), true
	}
	if n, err := strconv.ParseUint(string(raw), 10, 64); err == nil {
		return strconv.FormatUint(n, 10), true
	}
	var f float64
	if err := json.Unmarshal(raw, &f); err == nil {
		if f == float64(int64(f)) {
			return fmt.Sprintf("%d", int64(f)), true
		}
		return fmt.Sprintf("%g", f), true
	}
	var b bool
	if err := json.Unmarshal(raw, &b); err == nil {
		return strconv.FormatBool(b), true
	}
	return "", false
}

func comfyParamToDisplay(name string, raw json.RawMessage) *models.ComfyNodeParam {
	if string(raw) == "null" {
		return nil
	}
	if val, ok := comfyScalar(raw); ok {
		return &models.ComfyNodeParam{Name: name, Value: val, SearchTerm: comfyLinkTerm(name, val)}
	}
	if ref, ok := refNodeID(raw); ok {
		return &models.ComfyNodeParam{Name: name, Value: "→ " + ref, IsRef: true}
	}
	return &models.ComfyNodeParam{Name: name, Value: string(raw)}
}

// A fixed order: the passes are first-writer-wins, so map order would
// change the checkpoint, LoRA order and hash between parses.
func comfyNodeIDs[T any](nodes map[string]T) []string {
	ids := slices.Collect(maps.Keys(nodes))
	sortComfyKeys(ids)
	return ids
}

func sortComfyKeys(keys []string) {
	slices.SortFunc(keys, func(a, b string) int {
		ai, bi := parseIntKey(a), parseIntKey(b)
		if ai >= 0 && bi >= 0 {
			return cmp.Compare(ai, bi)
		}
		if ai >= 0 {
			return -1
		}
		if bi >= 0 {
			return 1
		}
		return cmp.Compare(a, b)
	})
}

func parseIntKey(s string) int {
	if s == "" {
		return -1
	}
	n := 0
	for _, c := range s {
		if c < '0' || c > '9' {
			return -1
		}
		n = n*10 + int(c-'0')
	}
	return n
}

// An empty key is a role the node class leaves unset.
func nodeInput[T any](inputs map[string]json.RawMessage, key string) (T, bool) {
	var v T
	raw, ok := inputs[key]
	if !ok || key == "" {
		return v, false
	}
	if err := json.Unmarshal(raw, &v); err != nil {
		return v, false
	}
	return v, true
}

// comfyNodeRoles names, for one node class, the input each recipe column
// reads. Lora with a %02d verb is a numbered stack of LoraCount slots.
type comfyNodeRoles struct {
	Prompt    string `json:"prompt,omitempty"`
	Positive  string `json:"positive,omitempty"`
	Seed      string `json:"seed,omitempty"`
	Steps     string `json:"steps,omitempty"`
	CFG       string `json:"cfg,omitempty"`
	Sampler   string `json:"sampler,omitempty"`
	Scheduler string `json:"scheduler,omitempty"`
	Model     string `json:"model,omitempty"`
	Lora      string `json:"lora,omitempty"`
	LoraCount int    `json:"lora_count,omitempty"`
}

//go:embed comfyui_nodes.json
var defaultComfyNodesJSON []byte

type comfyNodesDoc struct {
	Version int                       `json:"version"`
	Nodes   map[string]comfyNodeRoles `json:"nodes"`
}

var defaultComfyNodes = func() map[string]comfyNodeRoles {
	var doc comfyNodesDoc
	if err := json.Unmarshal(defaultComfyNodesJSON, &doc); err != nil {
		panic("comfyui_nodes.json: " + err.Error())
	}
	return doc.Nodes
}()

var nodeOverlay struct {
	sync.Mutex
	path  string
	stamp string
	table map[string]comfyNodeRoles
}

// SetNodeOverlay names the operator's comfyui_nodes.json: its entries
// replace the shipped ones for the same class and add new classes.
func SetNodeOverlay(path string) {
	nodeOverlay.Lock()
	defer nodeOverlay.Unlock()
	nodeOverlay.path, nodeOverlay.stamp, nodeOverlay.table = path, "", nil
}

// The overlay is stat'ed per call, so an edit reaches the next ingest; a
// file that fails to parse is reported once per edit and ignored.
func comfyNodeTable() map[string]comfyNodeRoles {
	nodeOverlay.Lock()
	defer nodeOverlay.Unlock()
	fi, err := os.Stat(nodeOverlay.path)
	if nodeOverlay.path == "" || err != nil {
		return defaultComfyNodes
	}
	stamp := fi.ModTime().String() + "/" + strconv.FormatInt(fi.Size(), 10)
	if stamp == nodeOverlay.stamp {
		return nodeOverlay.table
	}
	nodeOverlay.stamp, nodeOverlay.table = stamp, defaultComfyNodes
	data, err := os.ReadFile(nodeOverlay.path)
	var doc comfyNodesDoc
	if err == nil {
		err = json.Unmarshal(data, &doc)
	}
	if err == nil && doc.Version != 1 {
		err = fmt.Errorf("version %d, want 1", doc.Version)
	}
	if err != nil {
		logx.Warnf("metadata: ignoring %s: %v", nodeOverlay.path, err)
		return defaultComfyNodes
	}
	merged := maps.Clone(defaultComfyNodes)
	maps.Copy(merged, doc.Nodes)
	nodeOverlay.table = merged
	return merged
}

// A column an earlier node filled is kept; loras accumulate.
func applyComfyNodeInputs(roles comfyNodeRoles, inputs map[string]json.RawMessage, meta *models.ComfyUIMetadata, nodes map[string]comfyAPINode) {
	if roles.Seed != "" && meta.Seed == nil {
		if seed, ok := resolveSeedInput(inputs[roles.Seed], nodes); ok {
			meta.Seed = &seed
		}
	}
	if steps, ok := nodeInput[int](inputs, roles.Steps); ok && meta.Steps == nil {
		meta.Steps = &steps
	}
	if cfg, ok := nodeInput[float64](inputs, roles.CFG); ok && meta.CFGScale == nil {
		meta.CFGScale = &cfg
	}
	// The scheduler belongs to the sampler of its own node: a later
	// sampler node keeps the column and must not append to it.
	if sampler, ok := nodeInput[string](inputs, roles.Sampler); ok && meta.Sampler == "" {
		meta.Sampler = sampler
		if scheduler, ok := nodeInput[string](inputs, roles.Scheduler); ok && scheduler != "" && sampler != "" {
			meta.Sampler += "/" + scheduler
		}
	}
	if model, ok := nodeInput[string](inputs, roles.Model); ok && meta.ModelCheckpoint == "" {
		meta.ModelCheckpoint = model
	}
	keys := []string{roles.Lora}
	if roles.LoraCount > 0 {
		keys = keys[:0]
		for i := 1; i <= roles.LoraCount; i++ {
			keys = append(keys, fmt.Sprintf(roles.Lora, i))
		}
	}
	for _, key := range keys {
		if lora, ok := nodeInput[string](inputs, key); ok && lora != "" && lora != "None" {
			appendCheckpoint(meta, lora)
		}
	}
}

func appendCheckpoint(meta *models.ComfyUIMetadata, name string) {
	if meta.ModelCheckpoint != "" {
		meta.ModelCheckpoint += " + " + name
		return
	}
	meta.ModelCheckpoint = name
}
