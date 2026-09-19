package interceptor

import (
	"context"
	"strings"
	"testing"

	"github.com/kstruzzieri/go-llm/agent"
)

func TestDefaultsAreSixUniqueInterceptors(t *testing.T) {
	names := make([]string, 0, 6)
	for _, ic := range Defaults() {
		names = append(names, ic.Name())
	}
	if got := strings.Join(names, ","); got != "zero_width,encoding,typoglycemia,invariants,egress,secrets" {
		t.Fatalf("names = %s", got)
	}
}

func BenchmarkDefaultsOn64KB(b *testing.B) {
	content := strings.Repeat("func main() { fmt.Println(\"hello\") } // ignore nothing here\n", 1100)
	in := inputOf(agent.OriginWorkspace, content)
	ics := Defaults()
	b.ResetTimer()
	for range b.N {
		for _, ic := range ics {
			if _, err := ic.InspectInput(context.Background(), in); err != nil {
				b.Fatal(err)
			}
		}
	}
}
