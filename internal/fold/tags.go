package fold

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/ubixsys/ubixshepherd/internal/git"
	"github.com/ubixsys/ubixshepherd/internal/store"
)

// Version is a release version: major.minor.patch.
type Version struct{ Major, Minor, Patch int }

func (v Version) String() string { return fmt.Sprintf("%d.%d.%d", v.Major, v.Minor, v.Patch) }

// Less orders versions.
func (v Version) Less(o Version) bool {
	if v.Major != o.Major {
		return v.Major < o.Major
	}
	if v.Minor != o.Minor {
		return v.Minor < o.Minor
	}
	return v.Patch < o.Patch
}

// Bump returns the next version for a bump: major, minor or patch.
func (v Version) Bump(kind string) (Version, error) {
	switch kind {
	case "major":
		return Version{v.Major + 1, 0, 0}, nil
	case "minor":
		return Version{v.Major, v.Minor + 1, 0}, nil
	case "patch":
		return Version{v.Major, v.Minor, v.Patch + 1}, nil
	}
	return v, refuse("bump %q: want major, minor or patch", kind)
}

var semver = regexp.MustCompile(`^(\d+)\.(\d+)\.(\d+)$`)

// ParseTag reads a release tag with the given prefix ("v1.2.3"); pre-releases and other
// tags are not releases and report false.
func ParseTag(prefix, tag string) (Version, bool) {
	if !strings.HasPrefix(tag, prefix) {
		return Version{}, false
	}
	m := semver.FindStringSubmatch(strings.TrimPrefix(tag, prefix))
	if m == nil {
		return Version{}, false
	}
	a, _ := strconv.Atoi(m[1])
	b, _ := strconv.Atoi(m[2])
	c, _ := strconv.Atoi(m[3])
	return Version{a, b, c}, true
}

// Next is the version a bump hands out: past the highest release tag on the remote and
// every live reservation, so two lanes can never be handed the same one.
func Next(prefix, bump string, remoteTags []string, reserved []string) (Version, error) {
	var top Version
	for _, t := range append(append([]string{}, remoteTags...), reserved...) {
		if v, ok := ParseTag(prefix, t); ok && top.Less(v) {
			top = v
		}
	}
	return top.Bump(bump)
}

// RemoteTags lists the remote's tags as they are now, not as the last fetch saw them.
func RemoteTags(ctx context.Context, repo string) ([]string, error) {
	if !git.HasRemote(ctx, repo, "origin") {
		out, err := git.Run(ctx, repo, "tag", "--list")
		if err != nil || out == "" {
			return nil, err
		}
		return strings.Split(out, "\n"), nil
	}
	out, err := git.Run(ctx, repo, "ls-remote", "--tags", "--refs", "origin")
	if err != nil {
		return nil, err
	}
	var tags []string
	for _, line := range strings.Split(out, "\n") {
		if _, ref, ok := strings.Cut(line, "\t"); ok {
			tags = append(tags, strings.TrimPrefix(ref, "refs/tags/"))
		}
	}
	return tags, nil
}

// Reserve hands the next version for a bump to a lane. The repo's lock makes it atomic:
// the remote's tags and the live reservations are read and the new one written in one
// step, so a concurrent reserve in the same repo gets the next number.
func (f *Fold) Reserve(ctx context.Context, repoID, laneID int64, bump string) (store.Reservation, error) {
	repo, err := f.Store.Repo(ctx, repoID)
	if err != nil {
		return store.Reservation{}, err
	}
	if laneID != 0 {
		lane, err := f.Store.Lane(ctx, laneID)
		if err != nil {
			return store.Reservation{}, err
		}
		if lane.RepoID != repo.ID || lane.State != store.LaneOpen {
			return store.Reservation{}, refuse("lane %s is not an open lane of %s", lane.Name, repo.Name)
		}
	}
	lock := f.repoLock(repo.ID)
	lock.Lock()
	defer lock.Unlock()

	prefix := f.Conf().Profile(repo.Name).TagPrefix
	tags, err := RemoteTags(ctx, repo.Path)
	if err != nil {
		return store.Reservation{}, err
	}
	live, err := f.Store.Reservations(ctx, repo.ID)
	if err != nil {
		return store.Reservation{}, err
	}
	var reserved []string
	for _, r := range live {
		reserved = append(reserved, r.Tag)
	}
	v, err := Next(prefix, bump, tags, reserved)
	if err != nil {
		return store.Reservation{}, err
	}
	return f.Store.CreateReservation(ctx, store.Reservation{RepoID: repo.ID, LaneID: laneID, Tag: prefix + v.String(), State: store.TagReserved})
}

// ReleaseTag gives a reservation back.
func (f *Fold) ReleaseTag(ctx context.Context, repoID int64, tag string) error {
	live, err := f.Store.Reservations(ctx, repoID)
	if err != nil {
		return err
	}
	for _, r := range live {
		if r.Tag == tag {
			return f.Store.SetReservation(ctx, r.ID, store.TagReleased, "")
		}
	}
	return refuse("%s is not reserved", tag)
}

// checkTag decides a release tag pushed from the repo: it must be reserved, from a lane
// by that lane, and once the reservation's lane has merged it must contain the merge. It
// returns a problem, or "" (and records the tag as pushed).
func (f *Fold) checkTag(ctx context.Context, repo store.Repo, lane *store.Lane, dir, tag, sha string) (string, error) {
	prefix := f.Conf().Profile(repo.Name).TagPrefix
	if _, ok := ParseTag(prefix, tag); !ok {
		return "", nil // not a release tag: not Shepherd's to judge
	}
	live, err := f.Store.Reservations(ctx, repo.ID)
	if err != nil {
		return "", err
	}
	for _, r := range live {
		if r.Tag != tag {
			continue
		}
		if lane != nil && r.LaneID != 0 && r.LaneID != lane.ID {
			other := "another lane"
			if l, err := f.Store.Lane(ctx, r.LaneID); err == nil {
				other = "lane " + l.Name
			}
			return fmt.Sprintf("%s is reserved by %s, not lane %s", tag, other, lane.Name), nil
		}
		state := store.TagPushed
		if r.LaneID != 0 {
			if lf, err := f.Store.LaneForge(ctx, r.LaneID); err == nil && lf.MergeSHA != "" {
				if !git.Ok(ctx, dir, "cat-file", "-e", lf.MergeSHA+"^{commit}") {
					git.Run(ctx, dir, "fetch", "--quiet", "origin")
				}
				if !git.Ok(ctx, dir, "merge-base", "--is-ancestor", lf.MergeSHA, sha) {
					return fmt.Sprintf("%s does not contain its lane's merge (%s): a release cut there would ship without the work. Tag a commit that includes it", tag, shortSHA(lf.MergeSHA)), nil
				}
				state = store.TagVerified
			}
		}
		return "", f.Store.SetReservation(ctx, r.ID, state, sha)
	}
	return fmt.Sprintf("%s is not reserved; reserve the next version first (shepherd tag reserve %s minor, or the tag_reserve tool), so no other session takes it", tag, repo.Name), nil
}

// VerifyTags checks a merged lane's pushed tags contain the merge: a tag cut before the
// merge landed ships a release without the work (the trap Shepherd exists to catch). It
// returns one line per problem.
func (f *Fold) VerifyTags(ctx context.Context, lane store.Lane, mergeSHA string) []string {
	repo, err := f.Store.Repo(ctx, lane.RepoID)
	if err != nil {
		return nil
	}
	live, err := f.Store.Reservations(ctx, repo.ID)
	if err != nil {
		return nil
	}
	var problems []string
	for _, r := range live {
		if r.LaneID != lane.ID || r.State != store.TagPushed || r.SHA == "" {
			continue
		}
		git.Run(ctx, repo.Path, "fetch", "--quiet", "origin")
		if !git.Ok(ctx, repo.Path, "merge-base", "--is-ancestor", mergeSHA, r.SHA) {
			problems = append(problems, fmt.Sprintf("tag %s (%s) does not contain the merge %s: the release was cut before the work landed", r.Tag, shortSHA(r.SHA), shortSHA(mergeSHA)))
			continue
		}
		f.Store.SetReservation(ctx, r.ID, store.TagVerified, r.SHA)
	}
	return problems
}

// SortReservations orders reservations by version, newest last.
func SortReservations(prefix string, rs []store.Reservation) {
	sort.Slice(rs, func(i, j int) bool {
		a, _ := ParseTag(prefix, rs[i].Tag)
		b, _ := ParseTag(prefix, rs[j].Tag)
		return a.Less(b)
	})
}
