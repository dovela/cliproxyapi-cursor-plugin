package main

import (
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

type parameter struct {
	ID    string `json:"id"`
	Value string `json:"value"`
}
type modelVariant struct {
	LegacySlug      string      `json:"legacySlug"`
	ParameterValues []parameter `json:"parameterValues"`
	IsMaxMode       bool        `json:"isMaxMode"`
	DefaultNonMax   bool        `json:"isDefaultNonMaxConfig"`
	DefaultMax      bool        `json:"isDefaultMaxConfig"`
}
type pickerModel struct {
	Name        string         `json:"name"`
	Display     string         `json:"clientDisplayName"`
	Hidden      bool           `json:"isHidden"`
	ChatOnly    bool           `json:"isChatOnly"`
	CmdK        bool           `json:"onlySupportsCmdK"`
	Agent       *bool          `json:"supportsAgent"`
	NonMax      *bool          `json:"supportsNonMaxMode"`
	Context     int            `json:"contextTokenLimit"`
	MaxContext  int            `json:"contextTokenLimitForMaxMode"`
	Variants    []modelVariant `json:"variants"`
	Definitions []struct {
		ID   string `json:"id"`
		Type struct {
			Enum struct {
				Values []struct {
					Value any `json:"value"`
				} `json:"values"`
			} `json:"enumParameter"`
			Boolean struct {
				Values []struct {
					Value any `json:"value"`
				} `json:"values"`
			} `json:"booleanParameter"`
		} `json:"parameterType"`
	} `json:"parameterDefinitions"`
	LegacySlugs []string `json:"legacySlugs"`
	IDAliases   []string `json:"idAliases"`
}
type modelEntry struct {
	ID, Name, Run, Size, PID string
	Context                  int
	Levels                   map[string]string
	Fast, Partial            bool
	Variants                 []modelVariant
	Default                  modelVariant
	Model                    pickerModel
}
type modelCache struct {
	At      time.Time
	Entries []modelEntry
}
type modelResolution struct {
	ID     string
	Params []parameter
	Max    bool
}

var effortOrder = []string{"none", "minimal", "low", "medium", "high", "xhigh", "max"}
var contextPattern = regexp.MustCompile(`(?i)^(\d+(?:\.\d+)?)([km])$`)

func contextTokens(value string) int {
	m := contextPattern.FindStringSubmatch(strings.TrimSpace(value))
	if len(m) != 3 {
		return 0
	}
	v, _ := strconv.ParseFloat(m[1], 64)
	if strings.EqualFold(m[2], "m") {
		v *= 1e6
	} else {
		v *= 1e3
	}
	return int(math.Round(v))
}
func values(v modelVariant) map[string]string {
	out := make(map[string]string, len(v.ParameterValues))
	for _, p := range v.ParameterValues {
		out[p.ID] = p.Value
	}
	return out
}
func levelName(s string) string {
	if s == "extra-high" {
		return "xhigh"
	}
	if s == "ultra" {
		return "max"
	}
	if s == "off" {
		return "none"
	}
	return s
}
func indexOf(ss []string, s string) int {
	for i, v := range ss {
		if v == s {
			return i
		}
	}
	return -1
}
func defaultVariant(vs []modelVariant) modelVariant {
	for _, v := range vs {
		if v.DefaultNonMax {
			return v
		}
	}
	for _, v := range vs {
		if v.DefaultMax {
			return v
		}
	}
	if len(vs) > 0 {
		return vs[0]
	}
	return modelVariant{}
}
func needsMax(m pickerModel, v modelVariant) bool {
	return m.NonMax != nil && !*m.NonMax || v.IsMaxMode
}
func parseModels(raw []byte, ids map[string]bool) ([]modelEntry, error) {
	var response struct {
		Models []pickerModel `json:"models"`
	}
	if err := json.Unmarshal(raw, &response); err != nil {
		return nil, failure(502, "Invalid Cursor model catalog")
	}
	entries := []modelEntry{}
	seen := map[string]bool{}
	skip := map[string]bool{"claude-4.5-haiku": true, "claude-4.5-haiku-thinking": true, "gemini-2.5-pro": true, "gemini-2.5-flash": true, "claude-haiku-4-5": true}
	for _, m := range response.Models {
		if m.Name == "" || m.Hidden || m.ChatOnly || m.CmdK || m.Agent != nil && !*m.Agent || skip[m.Name] {
			continue
		}
		defs := map[string][]string{}
		for _, d := range m.Definitions {
			vs := d.Type.Enum.Values
			if len(vs) == 0 {
				vs = d.Type.Boolean.Values
			}
			for _, v := range vs {
				defs[d.ID] = append(defs[d.ID], fmt.Sprint(v.Value))
			}
		}
		vs := m.Variants
		if ids != nil {
			kept := []modelVariant{}
			some := false
			for _, v := range vs {
				if v.LegacySlug == "" || ids[v.LegacySlug] {
					kept = append(kept, v)
					some = some || v.LegacySlug != ""
				}
			}
			if some {
				vs = kept
			}
		}
		partial := len(vs) < len(m.Variants)
		has := func(id, value string) bool {
			if !partial {
				return true
			}
			present := false
			for _, v := range vs {
				val, ok := values(v)[id]
				present = present || ok
				if ok && val == value {
					return true
				}
			}
			return !present
		}
		pid := ""
		levels := map[string]string{}
		for _, id := range []string{"effort", "reasoning_effort", "reasoning"} {
			if len(defs[id]) > 0 {
				pid = id
				break
			}
		}
		if pid == "" && indexOf(defs["thinking"], "true") >= 0 && indexOf(defs["thinking"], "false") >= 0 {
			pid = "thinking"
			levels = map[string]string{"none": "false", "high": "true"}
		} else {
			for _, v := range defs[pid] {
				l := levelName(v)
				if indexOf(effortOrder, l) >= 0 {
					levels[l] = v
				}
			}
		}
		for level, value := range levels {
			if !has(pid, value) {
				delete(levels, level)
			}
		}
		def := defaultVariant(vs)
		title := strings.Join(strings.Fields(strings.ReplaceAll(m.Display, "\u200b", "")), " ")
		if title == "" {
			title = m.Name
		}
		base := modelEntry{Run: m.Name, Name: title, PID: pid, Levels: levels, Variants: vs, Default: def, Model: m, Partial: partial}
		add := func(e modelEntry) {
			if seen[e.ID] {
				return
			}
			seen[e.ID] = true
			if e.Context <= 0 {
				e.Context = 200000
			}
			for _, v := range vs {
				p := values(v)
				if p["fast"] == "true" && (e.Size == "" || p["context"] == "" || p["context"] == e.Size) {
					e.Fast = true
				}
			}
			entries = append(entries, e)
		}
		contexts := []string{}
		for _, size := range defs["context"] {
			if has("context", size) {
				contexts = append(contexts, size)
			}
		}
		if len(contexts) > 0 {
			for _, size := range contexts {
				e := base
				e.ID = m.Name + "@" + size
				e.Name = title + " @ " + size
				e.Size = size
				e.Context = contextTokens(size)
				add(e)
			}
		} else {
			e := base
			e.ID = m.Name
			if e.ID == "default" {
				e.ID = "auto"
			}
			e.Context = m.Context
			if needsMax(m, def) && m.MaxContext > 0 {
				e.Context = m.MaxContext
			}
			add(e)
		}
	}
	if len(entries) == 0 {
		return nil, failure(502, "Cursor listed no models")
	}
	return entries, nil
}
func (p *pluginRuntime) catalog(s *requestScope, token string) ([]modelEntry, error) {
	key := digest(token)
	p.mu.Lock()
	cached, ok := p.models[key]
	p.mu.Unlock()
	if ok && p.now().Sub(cached.At) < 10*time.Minute {
		return cached.Entries, nil
	}
	var usable map[string]any
	var usableErr error
	ready := make(chan struct{})
	go func() {
		usable, usableErr = s.jsonClient(s.ctx, http.MethodPost, "/agent.v1.AgentService/GetUsableModels", token, map[string]any{}, catalogClientType)
		close(ready)
	}()
	picker, err := s.jsonClient(s.ctx, http.MethodPost, "/aiserver.v1.AiService/AvailableModels", token, map[string]any{"useModelParameters": true, "doNotUseMarkdown": true}, catalogClientType)
	<-ready
	if err != nil {
		return nil, err
	}
	var ids map[string]bool
	if usableErr == nil {
		for _, raw := range array(usable["models"]) {
			m := object(raw)
			for _, id := range []string{str(m["modelId"]), str(m["displayModelId"])} {
				if id != "" {
					if ids == nil {
						ids = map[string]bool{}
					}
					ids[id] = true
				}
			}
		}
	}
	entries, err := parseModels(encodeJSON(picker), ids)
	if err != nil {
		return nil, err
	}
	p.mu.Lock()
	if len(p.models) >= 256 {
		p.models = map[string]modelCache{}
	}
	p.models[key] = modelCache{At: p.now(), Entries: entries}
	p.mu.Unlock()
	return entries, nil
}
func (e modelEntry) info() any {
	levels := []string{}
	for _, l := range effortOrder {
		if _, ok := e.Levels[l]; ok {
			levels = append(levels, l)
		}
	}
	info := map[string]any{"ID": "cursor/" + e.ID, "Object": "model", "OwnedBy": "cursor", "Type": "cursor", "Name": e.ID, "DisplayName": e.Name, "ContextLength": e.Context, "InputTokenLimit": e.Context, "MaxCompletionTokens": 0, "SupportedGenerationMethods": []string{"chat"}, "SupportedInputModalities": []string{"text", "image"}, "SupportedOutputModalities": []string{"text"}, "SupportedParameters": []string{"tools", "tool_choice", "reasoning_effort", "service_tier"}}
	if len(levels) > 0 {
		info["Thinking"] = map[string]any{"Levels": levels, "ZeroAllowed": indexOf(levels, "none") >= 0, "DynamicAllowed": false}
	}
	return info
}
func fitEffort(want string, levels map[string]string) string {
	want = levelName(want)
	if _, ok := levels[want]; ok {
		return want
	}
	at := indexOf(effortOrder, want)
	if at < 0 {
		return ""
	}
	best := ""
	distance := 100
	for _, level := range effortOrder {
		if _, ok := levels[level]; !ok || level == "none" {
			continue
		}
		d := int(math.Abs(float64(indexOf(effortOrder, level) - at)))
		if d <= distance {
			distance = d
			best = level
		}
	}
	return best
}
func nearestVariant(vs []modelVariant, want map[string]string) *modelVariant {
	var best *modelVariant
	most := -1
	for i := range vs {
		have := values(vs[i])
		score := 0
		for k, val := range want {
			if have[k] == val {
				score++
			}
		}
		if have["context"] == want["context"] {
			score += 100
		}
		if score > most {
			most = score
			best = &vs[i]
		}
	}
	return best
}
func splitModelID(id string) (string, string) {
	fast, thinking := false, false
	cut := func(suffix string) bool {
		if strings.HasSuffix(id, suffix) && len(id) > len(suffix) {
			id = strings.TrimSuffix(id, suffix)
			return true
		}
		return false
	}
	fast = cut("-fast")
	thinking = cut("-thinking")
	effort := ""
	for _, word := range []string{"extra-high", "xhigh", "minimal", "none", "low", "medium", "high", "max"} {
		if cut("-" + word) {
			effort = levelName(word)
			break
		}
	}
	if !thinking {
		thinking = cut("-thinking")
	}
	if thinking {
		id += "-thinking"
	}
	if fast {
		id += "-fast"
	}
	return id, effort
}
func namedModel(entries []modelEntry, id string) (*modelEntry, *modelVariant, string, bool) {
	unique := []*modelEntry{}
	seen := map[string]bool{}
	for i := range entries {
		if !seen[entries[i].Run] {
			seen[entries[i].Run] = true
			unique = append(unique, &entries[i])
		}
	}
	at := func(e *modelEntry, v modelVariant) *modelEntry {
		size := values(v)["context"]
		for i := range entries {
			if entries[i].Run == e.Run && entries[i].Size == size {
				return &entries[i]
			}
		}
		return e
	}
	for _, e := range unique {
		vs := []modelVariant{}
		for _, v := range e.Variants {
			if v.LegacySlug == id {
				vs = append(vs, v)
			}
		}
		if seed := nearestVariant(vs, values(e.Default)); seed != nil {
			return at(e, *seed), seed, "", values(*seed)["fast"] == "true"
		}
	}
	family, effort := splitModelID(id)
	for _, e := range unique {
		vs := []modelVariant{}
		for _, v := range e.Variants {
			base, _ := splitModelID(v.LegacySlug)
			if v.LegacySlug != "" && base == family {
				vs = append(vs, v)
			}
		}
		if seed := nearestVariant(vs, values(e.Default)); seed != nil {
			return at(e, *seed), seed, effort, values(*seed)["fast"] == "true"
		}
	}
	for _, e := range unique {
		if e.Model.Name == id || indexOf(e.Model.LegacySlugs, id) >= 0 || indexOf(e.Model.IDAliases, id) >= 0 {
			return at(e, e.Default), nil, "", false
		}
	}
	return nil, nil, "", false
}
func resolveModel(entries []modelEntry, id, effort, tier string) modelResolution {
	id = strings.TrimPrefix(id, "cursor/")
	var selected *modelEntry
	var seed *modelVariant
	for i := range entries {
		if entries[i].ID == id {
			selected = &entries[i]
			break
		}
	}
	legacyFast := false
	if selected == nil {
		var inheritedEffort string
		selected, seed, inheritedEffort, legacyFast = namedModel(entries, id)
		if effort == "" {
			effort = inheritedEffort
		}
	}
	if selected == nil {
		if id == "auto" {
			id = "default"
		}
		return modelResolution{ID: id}
	}
	e := *selected
	v := e.Default
	if seed != nil {
		v = *seed
	}
	want := values(v)
	if e.Size != "" {
		want["context"] = e.Size
	}
	effort = levelName(effort)
	if e.PID != "" && effort != "" {
		if e.PID != "thinking" && effort == "none" && e.Levels["none"] == "" && want["thinking"] != "" {
			want["thinking"] = "false"
		} else if at := fitEffort(effort, e.Levels); at != "" {
			want[e.PID] = e.Levels[at]
			if seed == nil && e.PID != "thinking" && want["thinking"] != "" {
				want["thinking"] = "true"
			}
		}
	}
	hasFast := false
	for _, d := range e.Model.Definitions {
		hasFast = hasFast || d.ID == "fast"
	}
	if hasFast {
		fast := tier == "priority" || tier == "fast"
		fast = fast || legacyFast
		want["fast"] = strconv.FormatBool(fast)
	}
	var chosen *modelVariant
	for i := range e.Variants {
		have := values(e.Variants[i])
		same := true
		for k, val := range want {
			if got, ok := have[k]; ok && got != val {
				same = false
				break
			}
		}
		if same {
			chosen = &e.Variants[i]
			break
		}
	}
	branchAllowed := false
	if seed != nil && e.PID != "" && e.PID != "thinking" {
		def := values(e.Default)
		for _, variant := range e.Variants {
			if levelName(values(variant)[e.PID]) == levelName(want[e.PID]) {
				branchAllowed = true
			}
		}
		for k, val := range want {
			if k != e.PID && k != "context" && def[k] != "" && def[k] != val {
				branchAllowed = true
			}
		}
	}
	if chosen == nil && branchAllowed {
		candidates := map[string]string{}
		for _, variant := range e.Variants {
			have := values(variant)
			compatible := have[e.PID] != ""
			for k, val := range want {
				if k != e.PID && have[k] != "" && have[k] != val {
					compatible = false
				}
			}
			if compatible {
				candidates[levelName(have[e.PID])] = have[e.PID]
			}
		}
		level := fitEffort(levelName(want[e.PID]), candidates)
		for i := range e.Variants {
			if levelName(values(e.Variants[i])[e.PID]) == level && level != "" {
				compatible := true
				for k, val := range want {
					if k != e.PID && values(e.Variants[i])[k] != "" && values(e.Variants[i])[k] != val {
						compatible = false
					}
				}
				if compatible {
					chosen = &e.Variants[i]
					break
				}
			}
		}
	}
	if chosen == nil && e.Partial {
		most := -1
		for i := range e.Variants {
			have := values(e.Variants[i])
			score := 0
			for k, val := range want {
				if have[k] == val {
					score++
				}
			}
			if have["context"] == want["context"] {
				score += 100
			}
			if score > most {
				chosen = &e.Variants[i]
				most = score
			}
		}
	}
	max := needsMax(e.Model, e.Default) || e.Model.Context > 0 && contextTokens(e.Size) > e.Model.Context
	resolved := e.Run
	if e.Default.LegacySlug != "" {
		resolved = e.Default.LegacySlug
	}
	if chosen != nil {
		for k, val := range values(*chosen) {
			want[k] = val
		}
		max = needsMax(e.Model, *chosen)
		if chosen.LegacySlug != "" {
			resolved = chosen.LegacySlug
		}
	}
	keys := make([]string, 0, len(want))
	for k := range want {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	params := make([]parameter, 0, len(keys))
	for _, k := range keys {
		params = append(params, parameter{k, want[k]})
	}
	return modelResolution{ID: resolved, Params: params, Max: max}
}
