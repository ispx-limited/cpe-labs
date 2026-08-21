package paramtree

import (
	"fmt"
	"sort"
	"strconv"

	"github.com/ispx-limited/cpe-labs/internal/cpeerr"
)

// Graft attaches every subtree of other that t does not already have,
// at the highest path where the two trees diverge, and returns the
// paths it attached, object paths with a trailing ".". Leaves and
// tables are attached as declared in other, so an AddObject on a
// grafted table works the same as on a profile-declared one. A path
// that is a leaf in both trees, or a leaf on one side and an object on
// the other, rejects the whole graft before anything is attached.
//
// This is how a software module's data model joins a running CPE: the
// module's fragment loads as a tree of its own, Graft places it under
// whatever it extends (Device. for a new vendor object, an existing
// table for new instances), and Unmount with the returned paths takes
// exactly that much out again. Observers see each attached object as
// ChangeObjectCreated and each attached leaf as ChangeValue, the way a
// USP ObjectCreation or ValueChange subscription expects.
func (t *Tree) Graft(other *Tree) ([]string, error) {
	if other == nil {
		return nil, cpeerr.Wrap("paramtree.Graft", cpeerr.KindInvalidArgument,
			fmt.Errorf("nil tree"))
	}
	other.mu.RLock()
	defer other.mu.RUnlock()

	type graftPoint struct {
		parent *Node
		name   string
		node   *Node
		path   []string
	}
	var points []graftPoint
	var collect func(dst, src *Node, prefix []string) error
	collect = func(dst, src *Node, prefix []string) error {
		for name, child := range src.children {
			path := append(append([]string{}, prefix...), name)
			existing, ok := dst.children[name]
			if !ok {
				points = append(points, graftPoint{parent: dst, name: name, node: child, path: path})
				continue
			}
			if existing.isLeaf() || child.isLeaf() {
				return fmt.Errorf("path %q already exists", joinPath(path))
			}
			if err := collect(existing, child, path); err != nil {
				return err
			}
		}
		return nil
	}

	var changes []Change
	t.mu.Lock()
	if err := collect(t.root, other.root, nil); err != nil {
		t.mu.Unlock()
		return nil, cpeerr.Wrap("paramtree.Graft", cpeerr.KindInvalidArgument, err)
	}
	// Attach in path order so observers see a deterministic sequence.
	sort.Slice(points, func(i, j int) bool { return joinPath(points[i].path) < joinPath(points[j].path) })
	roots := make([]string, 0, len(points))
	for _, p := range points {
		n := p.node.clone()
		p.parent.children[p.name] = n
		path := joinPath(p.path)
		if n.isLeaf() {
			roots = append(roots, path)
			changes = append(changes, Change{Path: path, New: *n.leaf, Kind: ChangeValue})
			continue
		}
		roots = append(roots, path+".")
		changes = append(changes, Change{Path: path + ".", Kind: ChangeObjectCreated})
	}
	t.mu.Unlock()

	if t.hasObservers() {
		for _, c := range changes {
			t.notify(c)
		}
	}
	return roots, nil
}

// Unmount removes the node at path, leaf or object, with everything
// beneath it, and reports an object removal to observers. Instances of
// a table are refused: DeleteObject owns those, because it also keeps
// the table's NumberOfEntries counter honest.
func (t *Tree) Unmount(path string) error {
	segments, err := parsePath(path)
	if err != nil {
		return err
	}
	if len(segments) == 0 {
		return cpeerr.Wrap("paramtree.Unmount", cpeerr.KindInvalidArgument,
			fmt.Errorf("cannot unmount root"))
	}

	var deleted string
	t.mu.Lock()
	parent, err := t.lookup(segments[:len(segments)-1])
	if err != nil {
		t.mu.Unlock()
		return cpeerr.Wrap("paramtree.Unmount", cpeerr.KindNotFound, err)
	}
	last := segments[len(segments)-1]
	if parent.isLeaf() {
		t.mu.Unlock()
		return cpeerr.Wrap("paramtree.Unmount", cpeerr.KindInvalidArgument,
			fmt.Errorf("path %q traverses a leaf", path))
	}
	n, ok := parent.children[last]
	if !ok {
		t.mu.Unlock()
		return cpeerr.Wrap("paramtree.Unmount", cpeerr.KindNotFound,
			fmt.Errorf("path %q not found", path))
	}
	if parent.table != nil {
		if i, convErr := strconv.Atoi(last); convErr == nil && i > 0 {
			t.mu.Unlock()
			return cpeerr.Wrap("paramtree.Unmount", cpeerr.KindInvalidArgument,
				fmt.Errorf("path %q is a table instance; use DeleteObject", path))
		}
	}
	delete(parent.children, last)
	if !n.isLeaf() && t.hasObservers() {
		deleted = joinPath(segments) + "."
	}
	t.mu.Unlock()

	if deleted != "" {
		t.notify(Change{Path: deleted, Kind: ChangeObjectDeleted})
	}
	return nil
}
