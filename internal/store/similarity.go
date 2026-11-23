package store

import (
	"errors"
	"math"
	"sort"
	"strings"
	"unicode"
)

var errProjectRequired = errors.New("explicit project required for similarity scan")

// SimilarMemoryGroup is a read-only consolidation hint. It never changes
// knowledge; the operator chooses a target and runs memory consolidate.
type SimilarMemoryGroup struct {
	Observations    []Observation
	Score           float64
	SuggestedTarget int64
}

type similarityDoc struct {
	observation Observation
	tokens      map[string]struct{}
}

// SimilarMemoryGroups clusters active observations in one project by lexical
// overlap (CJK bigrams + Latin words). It is a bounded, deterministic hint,
// not a semantic model: the operator still decides what to merge.
func (s *Store) SimilarMemoryGroups(project string, threshold float64, limit int) ([]SimilarMemoryGroup, error) {
	project, _ = NormalizeProject(project)
	project = strings.TrimSpace(project)
	if project == "" {
		return nil, errProjectRequired
	}
	if math.IsNaN(threshold) || threshold <= 0 || threshold >= 1 {
		threshold = 0.6
	}
	if limit <= 0 {
		limit = 20
	}
	if limit > 50 {
		limit = 50
	}
	recent, err := s.RecentObservations(project, "", 400)
	if err != nil {
		return nil, err
	}
	docs := make([]similarityDoc, 0, len(recent))
	for _, observation := range recent {
		if observation.State() != ObservationStateActive {
			continue
		}
		text := observation.Title + " " + observation.EffectiveAbstract() + " " + observation.Content
		tokens := similarityTokens(text)
		if len(tokens) < 3 {
			continue
		}
		docs = append(docs, similarityDoc{observation: observation, tokens: tokens})
	}

	parent := make([]int, len(docs))
	best := make([]float64, len(docs))
	for i := range parent {
		parent[i] = i
		best[i] = 0
	}
	var find func(int) int
	find = func(i int) int {
		for parent[i] != i {
			parent[i] = parent[parent[i]]
			i = parent[i]
		}
		return i
	}
	union := func(a, b int) {
		ra, rb := find(a), find(b)
		if ra != rb {
			parent[rb] = ra
		}
	}
	for i := 0; i < len(docs); i++ {
		for j := i + 1; j < len(docs); j++ {
			score := memorySimilarity(docs[i].tokens, docs[j].tokens)
			if score < threshold {
				continue
			}
			union(i, j)
			if score > best[i] {
				best[i] = score
			}
			if score > best[j] {
				best[j] = score
			}
		}
	}
	groups := map[int][]int{}
	for i := range docs {
		root := find(i)
		groups[root] = append(groups[root], i)
	}
	out := make([]SimilarMemoryGroup, 0, len(groups))
	for _, members := range groups {
		if len(members) < 2 {
			continue
		}
		sort.Ints(members)
		group := SimilarMemoryGroup{Score: 0}
		for _, index := range members {
			group.Observations = append(group.Observations, docs[index].observation)
			if best[index] > group.Score {
				group.Score = best[index]
			}
		}
		// Suggest the longest body as the seed for a refined target.
		longest := -1
		for _, observation := range group.Observations {
			if length := len([]rune(observation.Content)); length > longest {
				longest = length
				group.SuggestedTarget = observation.ID
			}
		}
		out = append(out, group)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Score != out[j].Score {
			return out[i].Score > out[j].Score
		}
		return len(out[i].Observations) > len(out[j].Observations)
	})
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func memorySimilarity(a, b map[string]struct{}) float64 {
	if len(a) == 0 || len(b) == 0 {
		return 0
	}
	small, large := a, b
	if len(b) < len(a) {
		small, large = b, a
	}
	inter := 0
	for token := range small {
		if _, ok := large[token]; ok {
			inter++
		}
	}
	if inter == 0 {
		return 0
	}
	jaccard := float64(inter) / float64(len(a)+len(b)-inter)
	containment := float64(inter) / float64(len(small))
	// Containment catches a short note embedded in a long one; cap it so a
	// generic title cannot match everything.
	score := jaccard
	if containment*0.9 > score {
		score = containment * 0.9
	}
	return score
}

func similarityTokens(text string) map[string]struct{} {
	tokens := map[string]struct{}{}
	runes := []rune(strings.ToLower(text))
	var word []rune
	var han []rune
	flushWord := func() {
		if len(word) >= 2 {
			tokens[string(word)] = struct{}{}
		}
		word = word[:0]
	}
	flushHan := func() {
		for i := 0; i+1 < len(han); i++ {
			tokens[string(han[i:i+2])] = struct{}{}
		}
		han = han[:0]
	}
	for _, r := range runes {
		switch {
		case unicode.Is(unicode.Han, r):
			flushWord()
			han = append(han, r)
		case unicode.IsLetter(r) || unicode.IsDigit(r):
			flushHan()
			word = append(word, r)
		default:
			flushWord()
			flushHan()
		}
	}
	flushWord()
	flushHan()
	return tokens
}
