// jsonw.go — a small, fast JSON writer for the forge's fixed-shape REST views, plus the
// value-coercion helpers the surfaces share when reading request bodies (encoding/json
// still does the parsing; this file is about not paying map/reflection costs on output).

package main

import (
	"bytes"
	"encoding/json"
	"strconv"
	"strings"
)

type jsonBuf struct{ bytes.Buffer }

func (b *jsonBuf) raw(s string) *jsonBuf { b.WriteString(s); return b }
func (b *jsonBuf) str(s string) *jsonBuf {
	enc, _ := json.Marshal(s)
	b.Write(enc)
	return b
}
func (b *jsonBuf) num(n int) *jsonBuf { b.WriteString(strconv.Itoa(n)); return b }
func (b *jsonBuf) boolean(v bool) *jsonBuf {
	if v {
		b.WriteString("true")
	} else {
		b.WriteString("false")
	}
	return b
}
func (b *jsonBuf) null() *jsonBuf      { b.WriteString("null"); return b }
func (b *jsonBuf) key(name string) *jsonBuf {
	b.str(name)
	b.WriteByte(':')
	return b
}
func (b *jsonBuf) bytes() []byte { return b.Buffer.Bytes() }

// ---- request-body value coercions (mirroring the TS typeof checks) -------------

func asString(v any) (string, bool) {
	s, ok := v.(string)
	return s, ok
}

func asBool(v any) (bool, bool) {
	b, ok := v.(bool)
	return b, ok
}

// JS Number(...) for the shapes the suite sends: numbers pass through, numeric
// strings parse, everything else is NaN (reported as ok=false).
func jsNumber(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case string:
		if n == "" {
			return 0, true
		}
		f, err := strconv.ParseFloat(strings.TrimSpace(n), 64)
		if err != nil {
			return 0, false
		}
		return f, true
	}
	return 0, false
}

// Parse a request body that must be a JSON object; nil when absent/invalid-shaped.
func parseObjectBody(raw []byte) map[string]any {
	if len(raw) == 0 {
		return nil
	}
	var parsed any
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return nil
	}
	obj, ok := parsed.(map[string]any)
	if !ok {
		return nil
	}
	return obj
}
