package httprequest

import (
	"strings"
	"testing"
)

// The topic graph is only useful if it is consistent: every edge must point at a
// topic that exists, ids must be unique and namespaced by the tool, and every
// topic must actually teach something. A dangling edge would send the model to a
// topic that returns an error mid-drill-down.
func TestTopicGraphIsWellFormed(t *testing.T) {
	ids := map[string]bool{}
	for _, topic := range topics {
		if topic.ID == "" || topic.Title == "" || strings.TrimSpace(topic.Body) == "" {
			t.Fatalf("a topic is missing its id, title, or body: %+v", topic)
		}
		if !strings.HasPrefix(topic.ID, Name+"/") {
			t.Fatalf("topic id %q is not namespaced by the tool", topic.ID)
		}
		if ids[topic.ID] {
			t.Fatalf("duplicate topic id %q", topic.ID)
		}
		ids[topic.ID] = true
	}

	for _, topic := range topics {
		for _, edge := range topic.Edges {
			if !ids[edge.To] {
				t.Fatalf("topic %q has an edge to %q, which does not exist", topic.ID, edge.To)
			}
			if edge.Type == "" || strings.TrimSpace(edge.When) == "" {
				t.Fatalf("topic %q has an edge to %q with no type or no when-condition", topic.ID, edge.To)
			}
			if edge.To == topic.ID {
				t.Fatalf("topic %q links to itself", topic.ID)
			}
		}
	}
}

// The graph must be reachable: the overview is the entry point, and every other
// topic should be reachable from it by following edges, or a topic is an island
// the model can never discover by drilling down.
func TestEveryTopicIsReachableFromOverview(t *testing.T) {
	byID := map[string][]string{}
	for _, topic := range topics {
		for _, edge := range topic.Edges {
			byID[topic.ID] = append(byID[topic.ID], edge.To)
		}
	}

	start := Name + "/overview"
	seen := map[string]bool{start: true}
	queue := []string{start}
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		for _, next := range byID[cur] {
			if !seen[next] {
				seen[next] = true
				queue = append(queue, next)
			}
		}
	}

	for _, topic := range topics {
		if !seen[topic.ID] {
			t.Fatalf("topic %q is not reachable from %q by following edges", topic.ID, start)
		}
	}
}
