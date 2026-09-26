package session

// ring keeps the most recent output bytes up to a fixed capacity so that
// newly attached clients can repaint the terminal.
type ring struct {
	buf []byte
	max int
}

func newRing(max int) *ring { return &ring{max: max} }

func (r *ring) Write(p []byte) {
	r.buf = append(r.buf, p...)
	if over := len(r.buf) - r.max; over > 0 {
		// 先頭を捨てる際はバッファを作り直し、古い領域を保持し続けないようにする
		r.buf = append(make([]byte, 0, r.max), r.buf[over:]...)
	}
}

func (r *ring) Bytes() []byte {
	out := make([]byte, len(r.buf))
	copy(out, r.buf)
	return out
}
