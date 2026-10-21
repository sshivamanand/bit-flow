package history

import (
	"testing"
	"time"
)

func TestAppendAndLoadNewestFirst(t *testing.T) {
	t.Setenv("BITFLOW_CONFIG_DIR", t.TempDir())
	for i, name := range []string{"a", "b", "c"} {
		if err := Append(Record{Time: time.Unix(int64(i), 0), Dir: "sent", Name: name, Bytes: 100, Seconds: 2, State: "done"}); err != nil {
			t.Fatal(err)
		}
	}
	rs, err := Load(2)
	if err != nil || len(rs) != 2 || rs[0].Name != "c" || rs[1].Name != "b" {
		t.Fatalf("got %+v, %v", rs, err)
	}
	if rs[0].Rate() != 50 {
		t.Fatalf("rate %v", rs[0].Rate())
	}
}

func TestLoadMissing(t *testing.T) {
	t.Setenv("BITFLOW_CONFIG_DIR", t.TempDir())
	if rs, err := Load(0); err != nil || len(rs) != 0 {
		t.Fatalf("got %+v, %v", rs, err)
	}
}
