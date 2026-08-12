package treebuild

import (
	"context"
	"fmt"

	"github.com/enj/soapbox/tools/internal/gitcli"
	"github.com/enj/soapbox/tools/internal/gitgraph"
)

// TagOptions describes one deterministic annotated release tag.
type TagOptions struct {
	// Commit is the destination commit the tag names.
	Commit string
	// Name is the destination tag name, such as v0.36.1.
	Name string
	// Tagger is the identity recorded in the object, with a raw date. The date
	// comes from the upstream release so a regenerated tag is byte identical.
	Tagger gitcli.Signature
	// Message is the tag message, preserved verbatim.
	Message string
}

// WriteTag writes the annotated tag object for one release and reports its
// object name. No ref is created and none is moved.
//
// The object and the ref are separate steps because published tags are
// immutable. Writing the object answers "what would this release be" exactly,
// down to the object name, which is what lets a dry run compare the tag it would
// publish against one that already exists without having claimed a name. Giving
// the object a name in the ref namespace is a later decision that a publisher
// makes, and that it can refuse.
//
// A release tag is always annotated and always names a commit. A lightweight tag
// records no tagger and no date, so it could not reproduce a release, and it is
// not something this package can produce even by accident.
func WriteTag(ctx context.Context, git *gitcli.Runner, opts TagOptions) (string, error) {
	if err := gitgraph.ValidateSHA(opts.Commit); err != nil {
		return "", fmt.Errorf("treebuild tag %q: commit: %w", opts.Name, err)
	}
	if err := validateRawDate("tagger", opts.Tagger.Date); err != nil {
		return "", fmt.Errorf("treebuild tag %q: %w", opts.Name, err)
	}
	object, err := git.WriteTagObject(ctx, gitcli.TagObjectOptions{
		Object:  opts.Commit,
		Type:    "commit",
		Name:    opts.Name,
		Message: opts.Message,
		Tagger:  opts.Tagger,
	})
	if err != nil {
		return "", fmt.Errorf("treebuild tag: %w", err)
	}
	return object, nil
}
