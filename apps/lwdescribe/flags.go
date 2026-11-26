package lwdescribe

import (
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"strings"

	"github.com/lixenwraith/llmgate/gate"
)

// The flag language the model writes, built from lw --schema: one pattern a
// flag and type, keys in one order (required first, then a listener's host,
// then the schema's order) and values JSON carries without escapes. lw splits
// a key from its value at the first '=', so a value may hold '='; it may not
// hold a comma (lw's separator), a quote, a backslash or a control character.
const (
	strGo   = `[^,'"\\\x00-\x1f\x7f]+`
	strJSON = `[^,'"\\\n\t]+`
	intRE   = `[0-9]{1,10}`
	numRE   = `[0-9]{1,10}(\.[0-9]{1,6})?`
	nameRE  = `[a-z0-9][a-z0-9_-]{0,31}`
)

// schema is the part of lw --schema the language reads
type schema struct {
	Version string `json:"version"`
	Flow    []struct {
		Key     string `json:"key"`
		Summary string `json:"summary"`
		List    bool   `json:"list"`
		Keys    []key  `json:"keys"`
	} `json:"flow"`
	Sources []plugin `json:"sources"`
	Sinks   []plugin `json:"sinks"`
}

type plugin struct {
	Type    string `json:"type"`
	Side    string `json:"side"`
	Summary string `json:"summary"`
	Keys    []key  `json:"keys"`
}

type key struct {
	Key      string   `json:"key"`
	Kind     string   `json:"kind"`
	Default  string   `json:"default"`
	Required bool     `json:"required"`
	Enum     []string `json:"enum"`
	Hint     string   `json:"hint"`
	Keys     []key    `json:"keys"`
}

// leaf is a settable key by its dotted path
type leaf struct {
	path string
	key
}

func leaves(keys []key, prefix string) (out []leaf) {
	for _, k := range keys {
		if k.Kind == "table" {
			out = append(out, leaves(k.Keys, prefix+k.Key+".")...)
		} else {
			out = append(out, leaf{prefix + k.Key, k})
		}
	}
	return out
}

// ordered is keys in the language's order, which the catalog lists: the
// required ones, a listener's host (required here: lw's default is every
// address), the rest, then the tables' keys, each in the schema's order
func ordered(keys []key, listening bool) (must, may []leaf) {
	var host, tables []leaf
	for _, lf := range leaves(keys, "") {
		switch {
		case lf.Required:
			must = append(must, lf)
		case listening && lf.path == "host":
			lf.Required = true
			host = append(host, lf)
		case strings.Contains(lf.path, "."):
			tables = append(tables, lf)
		default:
			may = append(may, lf)
		}
	}
	return append(must, host...), append(may, tables...)
}

// sideOf is a plugin's side as the catalog names it; a side that listens has
// its host required (ordered)
var sideOf = map[string]string{"listener": "listens", "chain_listener": "listens", "dialer": "dials"}

func listens(p plugin) bool { return sideOf[p.Side] == "listens" }

// kindOf is a key kind's value pattern and its word in the catalog; any
// other kind is free text
var kindOf = map[string]struct{ re, word string }{
	"bool": {"(true|false)", "true|false"}, "integer": {intRE, "int"}, "number": {numRE, "num"},
}

// stageKeys splits a flow stage's keys: its types, when it has a type key,
// and the rest
func stageKeys(keys []key) (types []string, rest []key) {
	for _, k := range keys {
		if k.Key == "type" {
			types = k.Enum
		} else {
			rest = append(rest, k)
		}
	}
	return types, rest
}

// flagPattern is one line the language takes, as a regexp of Go's and as the
// JSON Schema pattern of the same text
type flagPattern struct {
	flag string
	re   *regexp.Regexp
	json string
}

type language struct {
	version  string
	patterns []flagPattern
	catalog  string
}

// flagOf names each flow stage's flag, the one table a new stage extends
var flagOf = map[string]string{"rate_limit": "rate-limit", "filters": "filter", "format": "format", "heartbeat": "heartbeat"}

func newLanguage(data []byte) (*language, error) {
	var s schema
	if err := json.Unmarshal(data, &s); err != nil {
		return nil, fmt.Errorf("lw --schema: %w", err)
	}
	l := &language{version: s.Version}
	l.add("pipeline", nameRE)
	for _, set := range []struct {
		flag    string
		plugins []plugin
	}{{"source", s.Sources}, {"sink", s.Sinks}} {
		for _, p := range set.plugins {
			must, may := ordered(p.Keys, listens(p))
			l.add(set.flag, typed(p.Type, must, may))
		}
	}
	for _, st := range s.Flow {
		flag, ok := flagOf[st.Key]
		if !ok {
			return nil, fmt.Errorf("lw --schema: flow stage %q has no flag here", st.Key)
		}
		types, rest := stageKeys(st.Keys)
		must, may := ordered(rest, false)
		if types == nil {
			l.add(flag, pairs(must, may))
			continue
		}
		for _, t := range types {
			l.add(flag, typed(t, must, may))
		}
	}
	l.catalog = catalog(s)
	return l, nil
}

// add renders a spec in both dialects: they differ only in the value class
func (l *language) add(flag, spec string) {
	line := "--" + flag + " '" + spec + "'"
	l.patterns = append(l.patterns, flagPattern{flag, regexp.MustCompile("^" + strings.ReplaceAll(line, "\x00", strGo) + "$"),
		"^" + strings.ReplaceAll(line, "\x00", strJSON) + "$"})
}

// typed is TYPE, alone or with ':' and its pairs
func typed(typ string, must, may []leaf) string {
	t := regexp.QuoteMeta(typ)
	switch {
	case len(must) > 0:
		return t + ":" + pairs(must, may)
	case len(may) > 0:
		return t + "(:" + pairs(nil, may) + ")?"
	}
	return t
}

// pairs is the mandatory pairs in order, then any of the rest in order;
// with none mandatory, the first one present leads (a run)
func pairs(must, may []leaf) string {
	if len(must) == 0 {
		return run(may)
	}
	var ms []string
	for _, lf := range must {
		ms = append(ms, pair(lf))
	}
	return strings.Join(ms, ",") + tail(may)
}

// run is one or more of the pairs, in order
func run(ls []leaf) string {
	var alts []string
	for i, lf := range ls {
		alts = append(alts, pair(lf)+tail(ls[i+1:]))
	}
	return "(" + strings.Join(alts, "|") + ")"
}

func tail(ls []leaf) string {
	var b strings.Builder
	for _, lf := range ls {
		b.WriteString("(," + pair(lf) + ")?")
	}
	return b.String()
}

// pair is key=value, a list's key repeated for each entry; "\x00" marks
// where the dialect's free-text class goes
func pair(lf leaf) string {
	v := "\x00"
	if k, ok := kindOf[lf.Kind]; ok {
		v = k.re
	}
	if len(lf.Enum) > 0 {
		var es []string
		for _, e := range lf.Enum {
			es = append(es, regexp.QuoteMeta(e))
		}
		v = "(" + strings.Join(es, "|") + ")"
	}
	p := regexp.QuoteMeta(lf.path) + "=" + v
	if lf.Kind == "list" {
		return p + "(," + p + ")*"
	}
	return p
}

// jsonSchema is the answer's shape for the model server: {"flags": [...]},
// each item a flag the language takes; [] declines. llama.cpp's grammar
// ignores prefixItems beside items, so check holds the --pipeline first.
// mode "array" keeps the shape without the patterns, "none" sends no format.
func (l *language) jsonSchema(mode string) (json.RawMessage, error) {
	items := map[string]any{"type": "string"}
	switch mode {
	case "patterns":
		var alts []any
		for _, p := range l.patterns {
			alts = append(alts, map[string]any{"type": "string", "pattern": p.json})
		}
		items = map[string]any{"anyOf": alts}
	case "array":
	case "none":
		return nil, nil
	default:
		return nil, fmt.Errorf("constrain %q: one of patterns, array, none", mode)
	}
	return json.Marshal(map[string]any{"type": "object", "additionalProperties": false, "required": []string{"flags"},
		"properties": map[string]any{"flags": map[string]any{"type": "array", "maxItems": 40, "items": items}}})
}

// check holds an answer to the language, as the schema would have: a
// --pipeline first, every line a flag of a known type with its keys in
// order and no character a terminal or reader would hide
func (l *language) check(flags []string) string {
	for i, f := range flags {
		if strings.ContainsFunc(f, gate.Hidden) {
			return fmt.Sprintf("line %d holds a control or format character", i+1)
		}
		if i == 0 && !l.patterns[0].re.MatchString(f) {
			return "the first line must be --pipeline 'NAME', lowercase letters, digits, _ and -"
		}
		if !slices.ContainsFunc(l.patterns, func(p flagPattern) bool { return p.re.MatchString(f) }) {
			return fmt.Sprintf("line %d, %s: not a flag the catalog allows: check the type, the keys and their order", i+1, f)
		}
	}
	return ""
}

// catalog is the language for the model to read: each type with its keys
// in the language's order, and each table of keys once, naming the types
// that take it
func catalog(s schema) string {
	var b strings.Builder
	type table struct {
		keys  string
		types []string
	}
	var tables []*table
	section := func(title, flag string, plugins []plugin) {
		fmt.Fprintf(&b, "%s, --%s 'TYPE[:key=value,...]':\n", title, flag)
		for _, p := range plugins {
			side := ""
			if s := sideOf[p.Side]; s != "" {
				side = " (" + s + ")"
			}
			must, may := ordered(p.Keys, listens(p))
			var entries []string
			for _, lf := range append(must, may...) {
				if !strings.Contains(lf.path, ".") {
					entries = append(entries, keyEntry(lf.path, lf.key))
				}
			}
			for _, k := range p.Keys {
				if k.Kind != "table" {
					continue
				}
				var ks []string
				for _, lf := range leaves(k.Keys, k.Key+".") {
					ks = append(ks, keyEntry(lf.path, lf.key))
				}
				text := strings.Join(ks, ", ")
				i := slices.IndexFunc(tables, func(t *table) bool { return t.keys == text })
				if i < 0 {
					tables, i = append(tables, &table{keys: text}), len(tables)
				}
				tables[i].types = append(tables[i].types, flag+" "+p.Type)
				entries = append(entries, fmt.Sprintf("%s.* (table %d)", k.Key, i+1))
			}
			fmt.Fprintf(&b, "- %s%s: %s.", p.Type, side, p.Summary)
			if len(entries) > 0 {
				fmt.Fprintf(&b, " %s", strings.Join(entries, ", "))
			}
			b.WriteByte('\n')
		}
	}
	section("Sources", "source", s.Sources)
	section("Sinks", "sink", s.Sinks)
	b.WriteString("Flow, each optional, in this order:\n")
	for _, st := range s.Flow {
		enum, rest := stageKeys(st.Keys)
		types := ""
		if enum != nil {
			types = strings.Join(enum, "|") + ":"
		}
		must, may := ordered(rest, false)
		var ks []string
		for _, lf := range append(must, may...) {
			ks = append(ks, keyEntry(lf.path, lf.key))
		}
		many := ""
		if st.List {
			many = ", repeatable"
		}
		fmt.Fprintf(&b, "- --%s '%skey=value,...'%s: %s. %s\n", flagOf[st.Key], types, many, st.Summary, strings.Join(ks, ", "))
	}
	b.WriteString("Tables of keys, written dotted (tls.enabled=true), after the type's own keys:\n")
	for i, t := range tables {
		fmt.Fprintf(&b, "%d. For %s: %s\n", i+1, strings.Join(t.types, ", "), t.keys)
	}
	return b.String()
}

// keyEntry is one key for the catalog: name=KIND, then whether it repeats
// and its default or that it is required
func keyEntry(path string, k key) string {
	kind := kindOf[k.Kind].word
	if hint := map[string]string{"dir": "DIR", "file": "FILE", "glob": "GLOB", "regex": "RE2", "cidr": "CIDR", "host": "HOST", "path": "/PATH"}[k.Hint]; hint != "" {
		kind = hint
	}
	if len(k.Enum) > 0 {
		kind = strings.Join(k.Enum, "|")
	}
	if kind == "" {
		kind = "text"
	}
	var notes []string
	if k.Kind == "list" {
		notes = append(notes, "repeatable")
	}
	switch {
	case k.Required:
		notes = append(notes, "required")
	case k.Default != "":
		notes = append(notes, "default "+k.Default)
	}
	if len(notes) > 0 {
		kind += " (" + strings.Join(notes, ", ") + ")"
	}
	return path + "=" + kind
}
