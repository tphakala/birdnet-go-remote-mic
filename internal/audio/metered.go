package audio

// Observer receives the raw S16LE bytes of each captured period. It exists so
// the audio package can feed a level meter without importing the levels package
// (which would create an import cycle); levels.Meter satisfies it.
type Observer interface {
	Observe(pcm []byte)
}

// NewMeteredSource wraps inner so that every period it reads is also handed to
// obs before being returned. The observer runs on the caller's goroutine (the
// capture pump), so it must be cheap and non-blocking.
func NewMeteredSource(inner Source, obs Observer) Source {
	return &meteredSource{inner: inner, obs: obs}
}

type meteredSource struct {
	inner Source
	obs   Observer
}

func (m *meteredSource) Negotiated() (rate, channels int) { return m.inner.Negotiated() }

func (m *meteredSource) Read() (Period, error) {
	p, err := m.inner.Read()
	if err == nil {
		m.obs.Observe(p.Buf)
	}
	return p, err
}

func (m *meteredSource) Close() error { return m.inner.Close() }
