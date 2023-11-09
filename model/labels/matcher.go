// Copyright 2017 The Prometheus Authors
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
// http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package labels

import (
	"bytes"
	"strconv"
)

// MatchType is an enum for label matching types.
type MatchType int

// Possible MatchTypes.
const (
	MatchEqual MatchType = iota
	MatchNotEqual
	MatchRegexp
	MatchNotRegexp
	MatchLess
	MatchLessOrEqual
	MatchGreater
	MatchGreaterOrEqual
)

var matchTypeToStr = [...]string{
	MatchEqual:          "=",
	MatchNotEqual:       "!=",
	MatchRegexp:         "=~",
	MatchNotRegexp:      "!~",
	MatchLess:           "<",
	MatchLessOrEqual:    "<=",
	MatchGreater:        ">",
	MatchGreaterOrEqual: ">=",
}

func (m MatchType) String() string {
	if m < MatchEqual || m > MatchGreaterOrEqual {
		panic("unknown match type")
	}
	return matchTypeToStr[m]
}

func (m MatchType) IsLessGreaterTypeMatcher() bool {
	if m >= MatchLess && m <= MatchGreaterOrEqual {
		return true
	}
	return false
}

// Matcher models the matching of a label.
type Matcher struct {
	Type  MatchType
	Name  string
	Value string

	re *FastRegexMatcher
}

// NewMatcher returns a matcher object.
func NewMatcher(t MatchType, n, v string) (*Matcher, error) {
	m := &Matcher{
		Type:  t,
		Name:  n,
		Value: v,
	}
	if t == MatchRegexp || t == MatchNotRegexp {
		re, err := NewFastRegexMatcher(v)
		if err != nil {
			return nil, err
		}
		m.re = re
	}
	return m, nil
}

// MustNewMatcher panics on error - only for use in tests!
func MustNewMatcher(mt MatchType, name, val string) *Matcher {
	m, err := NewMatcher(mt, name, val)
	if err != nil {
		panic(err)
	}
	return m
}

func (m *Matcher) String() string {
	// Start a buffer with a pre-allocated size on stack to cover most needs.
	var bytea [1024]byte
	b := bytes.NewBuffer(bytea[:0])

	if m.shouldQuoteName() {
		b.Write(strconv.AppendQuote(b.AvailableBuffer(), m.Name))
	} else {
		b.WriteString(m.Name)
	}
	b.WriteString(m.Type.String())
	b.Write(strconv.AppendQuote(b.AvailableBuffer(), m.Value))

	return b.String()
}

func (m *Matcher) shouldQuoteName() bool {
	for i, c := range m.Name {
		if c == '_' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (i > 0 && c >= '0' && c <= '9') {
			continue
		}
		return true
	}
	return len(m.Name) == 0
}

// Matches returns whether the matcher matches the given string value.
func (m *Matcher) Matches(s string) bool {
	switch m.Type {
	case MatchEqual:
		return s == m.Value
	case MatchNotEqual:
		return s != m.Value
	case MatchRegexp:
		return m.re.MatchString(s)
	case MatchNotRegexp:
		return !m.re.MatchString(s)
	case MatchLess:
		return m.tryMatchesNumberElseString(s)
	case MatchLessOrEqual:
		return m.tryMatchesNumberElseString(s)
	case MatchGreater:
		return m.tryMatchesNumberElseString(s)
	case MatchGreaterOrEqual:
		return m.tryMatchesNumberElseString(s)
	}
	panic("labels.Matcher.Matches: invalid match type")
}

func (m *Matcher) tryMatchesNumberElseString(s string) bool {
	if sNum, err := strconv.ParseFloat(s, 64); err == nil {
		if vNum, err := strconv.ParseFloat(m.Value, 64); err == nil {
			switch m.Type {
			case MatchLess:
				return sNum < vNum
			case MatchLessOrEqual:
				return sNum <= vNum
			case MatchGreater:
				return sNum > vNum
			case MatchGreaterOrEqual:
				return sNum >= vNum
			default:
				panic("labels.Matcher.Matches: invalid code path for greater / less matching")
			}
		}
	}

	switch m.Type {
	case MatchLess:
		return s < m.Value
	case MatchLessOrEqual:
		return s <= m.Value
	case MatchGreater:
		return s > m.Value
	case MatchGreaterOrEqual:
		return s >= m.Value
	default:
		panic("labels.Matcher.Matches: invalid code path for greater / less matching")
	}
}

// Inverse returns a matcher that matches the opposite.
func (m *Matcher) Inverse() (*Matcher, error) {
	switch m.Type {
	case MatchEqual:
		return NewMatcher(MatchNotEqual, m.Name, m.Value)
	case MatchNotEqual:
		return NewMatcher(MatchEqual, m.Name, m.Value)
	case MatchRegexp:
		return NewMatcher(MatchNotRegexp, m.Name, m.Value)
	case MatchNotRegexp:
		return NewMatcher(MatchRegexp, m.Name, m.Value)
	case MatchLess:
		return NewMatcher(MatchGreaterOrEqual, m.Name, m.Value)
	case MatchLessOrEqual:
		return NewMatcher(MatchGreater, m.Name, m.Value)
	case MatchGreater:
		return NewMatcher(MatchLessOrEqual, m.Name, m.Value)
	case MatchGreaterOrEqual:
		return NewMatcher(MatchLess, m.Name, m.Value)
	}
	panic("labels.Matcher.Matches: invalid match type")
}

// GetRegexString returns the regex string.
func (m *Matcher) GetRegexString() string {
	if m.re == nil {
		return ""
	}
	return m.re.GetRegexString()
}

// SetMatches returns a set of equality matchers for the current regex matchers if possible.
// For examples the regexp `a(b|f)` will returns "ab" and "af".
// Returns nil if we can't replace the regexp by only equality matchers.
func (m *Matcher) SetMatches() []string {
	if m.re == nil {
		return nil
	}
	return m.re.SetMatches()
}

// Prefix returns the required prefix of the value to match, if possible.
// It will be empty if it's an equality matcher or if the prefix can't be determined.
func (m *Matcher) Prefix() string {
	if m.re == nil {
		return ""
	}
	return m.re.prefix
}

// IsRegexOptimized returns whether regex is optimized.
func (m *Matcher) IsRegexOptimized() bool {
	if m.re == nil {
		return false
	}
	return m.re.IsOptimized()
}
