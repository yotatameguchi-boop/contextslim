package pipeline

import (
	"strings"
	"testing"

	"github.com/yotatameguchi-boop/contextslim/internal/config"
)

// BenchmarkProcess1MiB measures parse+compress+validate+save on a ~1 MiB
// pytest -v log (design 6.2 target: p95 < 100 ms including process start).
func BenchmarkProcess1MiB(b *testing.B) {
	log := readLog(b, "pytest-v-fail")
	lines := strings.Split(log, "\n")
	var sb strings.Builder
	for _, l := range lines[:7] {
		sb.WriteString(l + "\n")
	}
	for sb.Len() < 1<<20 {
		sb.WriteString("tests/test_mod1.py::test_mod1_case1 PASSED                               [ 50%]\n")
	}
	pass := strings.Count(sb.String(), " PASSED ")
	sb.WriteString("\n============================== " + itoa(pass) + " passed in 1.00s ===============================\n")
	input := hookInput("bench", "pytest -v", sb.String(), nil)
	env := testEnv(b, config.ModeCompress)
	b.SetBytes(int64(sb.Len()))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		o := Process(env, input)
		if o.Output == nil {
			b.Fatalf("%+v", o.Event)
		}
	}
}

func itoa(n int) string {
	s := ""
	for n > 0 {
		s = string(rune('0'+n%10)) + s
		n /= 10
	}
	return s
}
