// Package steam finds installed Steam games so bit-flow can send one by name
// and drop it straight into the receiver's Steam library. A game is its
// folder under steamapps/common plus steamapps/appmanifest_<appid>.acf; with
// both in place, Steam shows the game as installed after a restart (and
// verifies or updates it as needed).
package steam

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
)

// Game is an installed Steam game.
type Game struct {
	AppID      string `json:"appid"`
	Name       string `json:"name"`
	InstallDir string `json:"installdir"`
	Size       int64  `json:"size"`
	Library    string `json:"library"`  // library root (contains steamapps/)
	Manifest   string `json:"manifest"` // …/steamapps/appmanifest_<appid>.acf
	Dir        string `json:"dir"`      // …/steamapps/common/<installdir>
}

// Roots lists the places Steam installs itself on this OS. BITFLOW_STEAM_ROOT overrides it.
func Roots() []string {
	if r := os.Getenv("BITFLOW_STEAM_ROOT"); r != "" {
		return filepath.SplitList(r)
	}
	home, _ := os.UserHomeDir()
	switch runtime.GOOS {
	case "darwin":
		return []string{filepath.Join(home, "Library", "Application Support", "Steam")}
	case "windows":
		var out []string
		for _, env := range []string{"ProgramFiles(x86)", "ProgramFiles"} {
			if p := os.Getenv(env); p != "" {
				out = append(out, filepath.Join(p, "Steam"))
			}
		}
		return append(out, `C:\Program Files (x86)\Steam`)
	default:
		return []string{
			filepath.Join(home, ".steam", "steam"),
			filepath.Join(home, ".local", "share", "Steam"),
			filepath.Join(home, ".var", "app", "com.valvesoftware.Steam", ".local", "share", "Steam"),
		}
	}
}

// Libraries returns every Steam library folder (each contains steamapps/).
func Libraries() []string {
	seen := map[string]bool{}
	var out []string
	add := func(p string) {
		p = filepath.Clean(p)
		if real, err := filepath.EvalSymlinks(p); err == nil {
			p = real
		}
		if seen[p] {
			return
		}
		if st, err := os.Stat(filepath.Join(p, "steamapps")); err == nil && st.IsDir() {
			seen[p] = true
			out = append(out, p)
		}
	}
	for _, root := range Roots() {
		add(root)
		b, err := os.ReadFile(filepath.Join(root, "steamapps", "libraryfolders.vdf"))
		if err != nil {
			continue
		}
		v, err := ParseVDF(string(b))
		if err != nil {
			continue
		}
		folders, _ := v.Get("libraryfolders").(map[string]any)
		for key, val := range folders {
			switch x := val.(type) {
			case map[string]any: // current format: "0" { "path" "…" }
				if p, ok := x["path"].(string); ok {
					add(p)
				}
			case string: // old format: "1" "D:\\SteamLibrary"
				if _, err := strconv.Atoi(key); err == nil {
					add(x)
				}
			}
		}
	}
	return out
}

// Games lists fully installed games in every library, sorted by name.
func Games() []Game {
	var games []Game
	for _, lib := range Libraries() {
		matches, _ := filepath.Glob(filepath.Join(lib, "steamapps", "appmanifest_*.acf"))
		for _, m := range matches {
			g, err := readManifest(m)
			if err != nil {
				continue
			}
			g.Library = lib
			if st, err := os.Stat(g.Dir); err != nil || !st.IsDir() {
				continue
			}
			games = append(games, g)
		}
	}
	sort.Slice(games, func(i, j int) bool { return strings.ToLower(games[i].Name) < strings.ToLower(games[j].Name) })
	return games
}

// stateFullyInstalled is the StateFlags bit Steam sets on a complete install.
const stateFullyInstalled = 4

func readManifest(path string) (Game, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return Game{}, err
	}
	v, err := ParseVDF(string(b))
	if err != nil {
		return Game{}, err
	}
	app, ok := v.Get("appstate").(map[string]any)
	if !ok {
		return Game{}, errors.New("no AppState")
	}
	str := func(k string) string { s, _ := app[k].(string); return s }
	flags, _ := strconv.Atoi(str("stateflags"))
	if flags&stateFullyInstalled == 0 {
		return Game{}, errors.New("not fully installed")
	}
	g := Game{AppID: str("appid"), Name: str("name"), InstallDir: str("installdir"), Manifest: path}
	g.Size, _ = strconv.ParseInt(str("sizeondisk"), 10, 64)
	if g.AppID == "" || g.InstallDir == "" || strings.ContainsAny(g.InstallDir, `/\`) || g.InstallDir == ".." {
		return Game{}, errors.New("incomplete manifest")
	}
	g.Dir = filepath.Join(filepath.Dir(path), "common", g.InstallDir)
	return g, nil
}

// Find returns games matching an app ID exactly or a name (case-insensitive;
// an exact name beats a partial one).
func Find(query string) ([]Game, error) {
	games := Games()
	if len(games) == 0 {
		return nil, errors.New("no installed Steam games found on this computer")
	}
	q := strings.ToLower(strings.TrimSpace(query))
	var exact, partial []Game
	for _, g := range games {
		n := strings.ToLower(g.Name)
		switch {
		case g.AppID == q || n == q:
			exact = append(exact, g)
		case strings.Contains(n, q):
			partial = append(partial, g)
		}
	}
	if len(exact) > 0 {
		return exact, nil
	}
	if len(partial) == 0 {
		return nil, fmt.Errorf("no installed Steam game matches %q (see `bitflow games`)", query)
	}
	return partial, nil
}

// ---- KeyValues (VDF) ----

// VDF is a parsed KeyValues block. Keys are lowercased (Steam treats them case-insensitively).
type VDF map[string]any

// Get returns a top-level value by (case-insensitive) key.
func (v VDF) Get(key string) any { return v[strings.ToLower(key)] }

// ParseVDF parses Valve's KeyValues text format: "key" "value" pairs and
// "key" { … } blocks, with // comments.
func ParseVDF(s string) (VDF, error) {
	p := &vdfParser{s: s}
	m, err := p.block(false)
	return VDF(m), err
}

type vdfParser struct {
	s   string
	pos int
}

func (p *vdfParser) block(nested bool) (map[string]any, error) {
	out := map[string]any{}
	for {
		tok, kind, err := p.next()
		if err != nil {
			return nil, err
		}
		switch kind {
		case tokEOF:
			if nested {
				return nil, errors.New("vdf: unexpected end of input")
			}
			return out, nil
		case tokClose:
			if !nested {
				return nil, errors.New("vdf: unexpected }")
			}
			return out, nil
		case tokOpen:
			return nil, errors.New("vdf: { without a key")
		}
		key := strings.ToLower(tok)
		val, vkind, err := p.next()
		if err != nil {
			return nil, err
		}
		switch vkind {
		case tokString:
			out[key] = val
		case tokOpen:
			child, err := p.block(true)
			if err != nil {
				return nil, err
			}
			out[key] = child
		default:
			return nil, fmt.Errorf("vdf: no value for %q", tok)
		}
	}
}

const (
	tokEOF = iota
	tokString
	tokOpen
	tokClose
)

func (p *vdfParser) next() (string, int, error) {
	for p.pos < len(p.s) {
		c := p.s[p.pos]
		switch {
		case c == ' ' || c == '\t' || c == '\r' || c == '\n':
			p.pos++
		case c == '/' && p.pos+1 < len(p.s) && p.s[p.pos+1] == '/':
			for p.pos < len(p.s) && p.s[p.pos] != '\n' {
				p.pos++
			}
		case c == '{':
			p.pos++
			return "", tokOpen, nil
		case c == '}':
			p.pos++
			return "", tokClose, nil
		case c == '"':
			return p.quoted()
		default: // unquoted token
			start := p.pos
			for p.pos < len(p.s) && !strings.ContainsRune(" \t\r\n{}\"", rune(p.s[p.pos])) {
				p.pos++
			}
			return p.s[start:p.pos], tokString, nil
		}
	}
	return "", tokEOF, nil
}

func (p *vdfParser) quoted() (string, int, error) {
	p.pos++ // opening quote
	var b strings.Builder
	for p.pos < len(p.s) {
		c := p.s[p.pos]
		switch {
		case c == '\\' && p.pos+1 < len(p.s):
			p.pos++
			switch e := p.s[p.pos]; e {
			case 'n':
				b.WriteByte('\n')
			case 't':
				b.WriteByte('\t')
			default:
				b.WriteByte(e)
			}
		case c == '"':
			p.pos++
			return b.String(), tokString, nil
		default:
			b.WriteByte(c)
		}
		p.pos++
	}
	return "", 0, errors.New("vdf: unterminated string")
}
