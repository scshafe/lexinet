package model

import (
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"
)

var (
	ErrTopicNameRequired = errors.New("topic name is required")
	ErrTopicSlugRequired = errors.New("topic slug is required")
	ErrInvalidTopicSlug  = errors.New("invalid topic slug")
)

type Topic struct {
	ID          uuid.UUID `json:"id"`
	Slug        string    `json:"slug"`
	Name        string    `json:"name"`
	Description *string   `json:"description,omitempty"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

type CreateTopicInput struct {
	Slug        string  `json:"slug,omitempty"`
	Name        string  `json:"name"`
	Description *string `json:"description,omitempty"`
}

type UpdateTopicInput struct {
	Slug        *string  `json:"slug,omitempty"`
	Name        *string  `json:"name,omitempty"`
	Description **string `json:"description,omitempty"`
}

type AssignTopicInput struct {
	Idea  string `json:"idea"`
	Topic string `json:"topic"`
}

type TopicSummary struct {
	ID          uuid.UUID `json:"id"`
	Slug        string    `json:"slug"`
	Name        string    `json:"name"`
	Description *string   `json:"description,omitempty"`
	IdeaCount   int       `json:"idea_count,omitempty"`
}

type TopicView struct {
	ID          uuid.UUID     `json:"id"`
	Slug        string        `json:"slug"`
	Name        string        `json:"name"`
	Description *string       `json:"description,omitempty"`
	IdeaCount   int           `json:"idea_count,omitempty"`
	Ideas       []IdeaSummary `json:"ideas,omitempty"`
	CreatedAt   time.Time     `json:"created_at"`
	UpdatedAt   time.Time     `json:"updated_at"`
}

type IdeaTopic struct {
	IdeaID    uuid.UUID `json:"idea_id"`
	TopicID   uuid.UUID `json:"topic_id"`
	Slug      string    `json:"slug"`
	Name      string    `json:"name"`
	CreatedAt time.Time `json:"created_at"`
}

type IdeaTopicsView struct {
	IdeaID uuid.UUID      `json:"idea_id"`
	Topics []TopicSummary `json:"topics"`
}

func NormalizeCreateTopicInput(input CreateTopicInput) (CreateTopicInput, error) {
	name, err := NormalizeTopicName(input.Name)
	if err != nil {
		return input, err
	}
	input.Name = name

	slugSource := input.Slug
	if strings.TrimSpace(slugSource) == "" {
		slugSource = name
	}
	slug, err := NormalizeTopicSlug(slugSource)
	if err != nil {
		return input, err
	}
	input.Slug = slug
	return input, nil
}

func NormalizeTopicName(name string) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return "", ErrTopicNameRequired
	}
	return name, nil
}

func NormalizeTopicSlug(slug string) (string, error) {
	slug = strings.ToLower(strings.TrimSpace(slug))
	if slug == "" {
		return "", ErrTopicSlugRequired
	}

	var b strings.Builder
	lastDash := false
	for _, r := range slug {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' {
			b.WriteRune(r)
			lastDash = false
			continue
		}
		if !lastDash {
			b.WriteByte('-')
			lastDash = true
		}
	}

	normalized := strings.Trim(b.String(), "-")
	if normalized == "" {
		return "", ErrInvalidTopicSlug
	}
	if !ValidTopicSlug(normalized) {
		return "", ErrInvalidTopicSlug
	}
	return normalized, nil
}

func ValidTopicSlug(slug string) bool {
	if slug == "" || slug[0] == '-' || slug[len(slug)-1] == '-' {
		return false
	}
	lastDash := false
	for _, r := range slug {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			lastDash = false
		case r == '-':
			if lastDash {
				return false
			}
			lastDash = true
		default:
			return false
		}
	}
	return true
}
