package skills_test

import (
	"context"
	"fmt"
)

// stubInspector stands in for the container runtime on the approval path.
//
// The production inspector asks the host what it holds under a digest. Tests
// need the same yes/no without a runtime, and they need to be able to say "the
// host holds something ELSE under that name" — the case that must refuse.
type stubInspector struct {
	// present maps a requested digest to what the host resolves it to. A digest
	// absent from the map is one this host does not have.
	present map[string]string
	// err, when set, is returned for every lookup: an unusable runtime.
	err error
}

// acceptAll answers every digest with itself, which is the ordinary case: the
// bytes are here, under the name they were approved by.
func acceptAll() *stubInspector { return &stubInspector{} }

func (s *stubInspector) VerifyImagePresent(_ context.Context, digest string) (string, error) {
	if s.err != nil {
		return "", s.err
	}
	if s.present == nil {
		return digest, nil
	}
	got, ok := s.present[digest]
	if !ok {
		return "", fmt.Errorf("%s is not present on this host", digest)
	}
	return got, nil
}

// countingInspector records how often the host was consulted, so a test can
// assert that approval looks once and does nothing else.
type countingInspector struct {
	inner *stubInspector
	calls int
}

func (c *countingInspector) VerifyImagePresent(ctx context.Context, digest string) (string, error) {
	c.calls++
	return c.inner.VerifyImagePresent(ctx, digest)
}
