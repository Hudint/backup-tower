package archive

import (
	"crypto/sha256"
	"encoding/hex"
	"hash"
	"io"
)

// Meter passes bytes through while counting and hashing them.
//
// It exists because an archive is not always produced where it is stored: when
// a helper container does the packing, the checksum has to be taken on the
// receiving side, over the bytes that actually arrived. Measuring at the
// destination is what makes the recorded checksum worth checking.
type Meter struct {
	w   io.Writer
	sum hash.Hash
	n   int64
}

// NewMeter wraps w.
func NewMeter(w io.Writer) *Meter {
	return &Meter{w: w, sum: sha256.New()}
}

func (m *Meter) Write(p []byte) (int, error) {
	n, err := m.w.Write(p)
	if n > 0 {
		m.sum.Write(p[:n])
		m.n += int64(n)
	}
	return n, err
}

// Bytes reports how much has passed through.
func (m *Meter) Bytes() int64 { return m.n }

// SHA256 reports the checksum of everything written so far.
func (m *Meter) SHA256() string { return hex.EncodeToString(m.sum.Sum(nil)) }
