// SPDX-License-Identifier: GPL-3.0-or-later

package previewcache

import (
	"bytes"
	"testing"
	"time"
)

func TestPutGetInvalidate(t *testing.T) {
	c := New(t.TempDir())
	k := Key{TrackID: 0xc01067da, Size: 4096, ModTime: time.Unix(1700000000, 123)}
	p := bytes.Repeat([]byte{7}, Size)
	if c.Get(k) != nil {
		t.Fatal("empty cache returned data")
	}
	if err := c.Put(k, p); err != nil {
		t.Fatal(err)
	}
	if got := c.Get(k); !bytes.Equal(got, p) {
		t.Fatal("round trip")
	}
	changed := k
	changed.Size++
	if c.Get(changed) != nil {
		t.Fatal("stale preview served for a changed file")
	}
	if c.Put(k, p[:10]) == nil {
		t.Fatal("short preview accepted")
	}
	// A new cache instance (restart) still finds it.
	if got := New(c.Dir).Get(k); !bytes.Equal(got, p) {
		t.Fatal("not persisted")
	}
}
