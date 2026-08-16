// Package paramtree is the protocol-agnostic, in-memory parameter tree
// every CWMP RPC and every USP operation reads from or writes to.
//
// One Tree per simulated CPE. Concurrent reads (Get, Names, Walk) are
// safe; writes (Set, AddObject, DeleteObject, AddTable, Mount)
// serialize via a single sync.RWMutex.
//
// Type identifiers live in this package; per-Type value validation
// lands in a separate change and is wired into Set there.
package paramtree

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/ispx-limited/cpe-labs/internal/cpeerr"
)

// Tree is the in-memory parameter tree for one simulated CPE.
type Tree struct {
	mu   sync.RWMutex
	root *Node
	obs  observers
}

// New returns an empty tree.
func New() *Tree {
	return &Tree{root: NewBranch()}
}

// Mount places n at the given path in the tree, creating any missing
// interior nodes along the way. Returns an error if the path is
// already occupied or if the path syntax is invalid.
func (t *Tree) Mount(path string, n *Node) error {
	segments, err := parsePath(path)
	if err != nil {
		return err
	}
	if len(segments) == 0 {
		return cpeerr.Wrap("paramtree.Mount", cpeerr.KindInvalidArgument,
			fmt.Errorf("cannot mount onto root"))
	}

	t.mu.Lock()
	defer t.mu.Unlock()

	parent := t.root
	for _, seg := range segments[:len(segments)-1] {
		if parent.isLeaf() {
			return cpeerr.Wrap("paramtree.Mount", cpeerr.KindInvalidArgument,
				fmt.Errorf("path %q traverses a leaf at %q", path, seg))
		}
		child, ok := parent.children[seg]
		if !ok {
			child = NewBranch()
			parent.children[seg] = child
		}
		parent = child
	}

	last := segments[len(segments)-1]
	if parent.isLeaf() {
		return cpeerr.Wrap("paramtree.Mount", cpeerr.KindInvalidArgument,
			fmt.Errorf("path %q traverses a leaf", path))
	}
	if _, exists := parent.children[last]; exists {
		return cpeerr.Wrap("paramtree.Mount", cpeerr.KindInvalidArgument,
			fmt.Errorf("path %q already occupied", path))
	}
	parent.children[last] = n
	return nil
}

// Get returns the Value at path. Returns cpeerr.KindNotFound if the
// path does not exist or names an interior (non-leaf) node.
func (t *Tree) Get(path string) (Value, error) {
	segments, err := parsePath(path)
	if err != nil {
		return Value{}, err
	}
	t.mu.RLock()
	defer t.mu.RUnlock()

	n, err := t.lookup(segments)
	if err != nil {
		return Value{}, cpeerr.Wrap("paramtree.Get", cpeerr.KindNotFound, err)
	}
	if !n.isLeaf() {
		return Value{}, cpeerr.Wrap("paramtree.Get", cpeerr.KindNotFound,
			fmt.Errorf("path %q is an interior node, not a leaf", path))
	}
	return *n.leaf, nil
}

// Set overwrites the Value at path. Returns cpeerr.KindInvalidArgument
// if the path is read-only or if the new Value's Type differs from the
// current Value's Type. Returns cpeerr.KindNotFound if the path does
// not exist or names an interior node.
func (t *Tree) Set(path string, v Value) error {
	segments, err := parsePath(path)
	if err != nil {
		return err
	}
	// Notification happens after the write lock drops, so an observer is free
	// to read or write the tree without deadlocking. change stays nil unless a
	// value actually moved.
	var change *Change
	defer func() {
		if change != nil {
			t.notify(*change)
		}
	}()

	t.mu.Lock()
	defer t.mu.Unlock()

	n, err := t.lookup(segments)
	if err != nil {
		return cpeerr.Wrap("paramtree.Set", cpeerr.KindNotFound, err)
	}
	if !n.isLeaf() {
		return cpeerr.Wrap("paramtree.Set", cpeerr.KindNotFound,
			fmt.Errorf("path %q is an interior node, not a leaf", path))
	}
	if !n.leaf.Writable {
		return cpeerr.Wrap("paramtree.Set", cpeerr.KindInvalidArgument,
			fmt.Errorf("path %q is read-only", path))
	}
	if n.leaf.Type != v.Type {
		return cpeerr.Wrap("paramtree.Set", cpeerr.KindInvalidArgument,
			fmt.Errorf("type mismatch at %q: have %s, got %s", path, n.leaf.Type, v.Type))
	}
	if err := Validate(v.Type, v.Raw); err != nil {
		return err
	}
	old := *n.leaf
	*n.leaf = v
	if t.hasObservers() && old.Raw != v.Raw {
		change = &Change{Path: path, Old: old, New: v, Kind: ChangeValue}
	}
	return nil
}

// SetSystem is like Set but bypasses the Writable check. Used for
// system-initiated mutations during init (e.g. fleet serial stamping
// where the operator's profile declares SerialNumber read-only).
// Preserves the existing leaf's Type and Writable; only Raw changes.
//
// Returns cpeerr.KindNotFound if path does not exist or names an
// interior node, and cpeerr.KindInvalidArgument if raw does not pass
// the leaf's type validation.
func (t *Tree) SetSystem(path, raw string) error {
	segments, err := parsePath(path)
	if err != nil {
		return err
	}
	var change *Change
	defer func() {
		if change != nil {
			t.notify(*change)
		}
	}()

	t.mu.Lock()
	defer t.mu.Unlock()

	n, err := t.lookup(segments)
	if err != nil {
		return cpeerr.Wrap("paramtree.SetSystem", cpeerr.KindNotFound, err)
	}
	if !n.isLeaf() {
		return cpeerr.Wrap("paramtree.SetSystem", cpeerr.KindNotFound,
			fmt.Errorf("path %q is an interior node, not a leaf", path))
	}
	if err := Validate(n.leaf.Type, raw); err != nil {
		return err
	}
	old := *n.leaf
	n.leaf.Raw = raw
	if t.hasObservers() && old.Raw != raw {
		change = &Change{Path: path, Old: old, New: *n.leaf, Kind: ChangeValue}
	}
	return nil
}

// Reset replaces the receiver's contents with those of other. Existing
// *Tree pointers held by callers (handlers, the inform builder, etc.)
// remain valid; concurrent readers see the swap atomically when the
// receiver's write lock is released. After Reset returns, other has
// been drained and should be discarded by the caller.
//
// Reset preserves the receiver's mutex (so the swap is safe under
// existing callers' locks); it copies only other's root pointer. All
// per-leaf Attributes carried on the receiver before Reset are
// discarded, the post-Reset tree starts with whatever Attributes the
// supplied other tree carries (typically none, since other is freshly
// loaded from a profile).
func (t *Tree) Reset(other *Tree) error {
	if other == nil {
		return cpeerr.Wrap("paramtree.Reset", cpeerr.KindInvalidArgument,
			fmt.Errorf("other tree is nil"))
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.root = other.root
	return nil
}

// Clone returns a deep copy of the tree: every interior node, leaf
// value, attribute set and table template is copied, so nothing mutable
// is shared with the receiver and a write to either side is invisible
// to the other.
//
// This exists because building a fleet by re-parsing the profile once
// per CPE does not scale. A realistic residential-gateway profile is a
// few hundred leaves spread over several YAML files, and at 200k CPEs
// that is 200k parses of the same bytes before the first device says
// anything to the ACS. Parsing once and cloning gives every CPE the
// same independent tree for a fraction of the work.
//
// The clone starts with NO observers registered. Observers are wiring
// belonging to whoever built the original (the USP agent's value-change
// notifier, for instance), and silently carrying them onto a copy would
// have one CPE's writes firing another CPE's notifications.
//
// Clone takes the read lock, so it is safe to call while other
// goroutines read the same tree, and safe to call concurrently from
// several goroutines building different CPEs from one template.
func (t *Tree) Clone() *Tree {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return &Tree{root: t.root.clone()}
}

// GetAttributes returns the Attributes stored at path. If no
// SetAttributes has run for path, returns the BBF defaults
// (Notification=0, AccessList=nil, wire output renders nil as
// ["Subscriber"]).
//
// Returns cpeerr.KindNotFound if path does not resolve or names an
// interior node (only leaves carry attributes).
func (t *Tree) GetAttributes(path string) (Attributes, error) {
	segments, err := parsePath(path)
	if err != nil {
		return Attributes{}, err
	}
	t.mu.RLock()
	defer t.mu.RUnlock()

	n, err := t.lookup(segments)
	if err != nil {
		return Attributes{}, cpeerr.Wrap("paramtree.GetAttributes", cpeerr.KindNotFound, err)
	}
	if !n.isLeaf() {
		return Attributes{}, cpeerr.Wrap("paramtree.GetAttributes", cpeerr.KindNotFound,
			fmt.Errorf("path %q is an interior node, not a leaf", path))
	}
	if n.attrs == nil {
		return Attributes{}, nil
	}
	out := *n.attrs
	if n.attrs.AccessList != nil {
		out.AccessList = append([]string(nil), n.attrs.AccessList...)
	}
	return out, nil
}

// SetAttributes overwrites the Attributes stored at path. The whole
// struct is replaced, callers wanting partial mutation must Get
// first, edit, then Set.
//
// Returns cpeerr.KindNotFound if path does not resolve or names an
// interior node.
func (t *Tree) SetAttributes(path string, attrs Attributes) error {
	segments, err := parsePath(path)
	if err != nil {
		return err
	}
	t.mu.Lock()
	defer t.mu.Unlock()

	n, err := t.lookup(segments)
	if err != nil {
		return cpeerr.Wrap("paramtree.SetAttributes", cpeerr.KindNotFound, err)
	}
	if !n.isLeaf() {
		return cpeerr.Wrap("paramtree.SetAttributes", cpeerr.KindNotFound,
			fmt.Errorf("path %q is an interior node, not a leaf", path))
	}
	stored := attrs
	if attrs.AccessList != nil {
		stored.AccessList = append([]string(nil), attrs.AccessList...)
	}
	n.attrs = &stored
	return nil
}

// Setter describes one (path, value) pair for SetBatch.
type Setter struct {
	Path  string
	Value Value
}

// BatchResult is one entry in SetBatch's result slice, in input order.
// OldValue is the leaf Value before the batch applied; NewValue is
// after. Changed reports whether OldValue.Raw and NewValue.Raw differ.
type BatchResult struct {
	Path     string
	OldValue Value
	NewValue Value
	Changed  bool
}

// SetBatchFailure categorises why a SetBatch entry failed.
type SetBatchFailure int

const (
	// FailureUnknown should not occur in practice; reserved as the
	// zero value of SetBatchFailure.
	FailureUnknown SetBatchFailure = iota
	// FailureNotFound: the path does not resolve or names an interior
	// node. Maps to CWMP fault 9005.
	FailureNotFound
	// FailureNotWritable: the leaf at path has Writable=false. Maps to
	// CWMP fault 9008.
	FailureNotWritable
	// FailureTypeMismatch: the new Value.Type differs from the leaf's
	// declared Type. Maps to CWMP fault 9007.
	FailureTypeMismatch
	// FailureInvalidValue: the new Value.Raw fails Validate(Type, Raw).
	// Maps to CWMP fault 9007.
	FailureInvalidValue
	// FailureDuplicatePath: the input contained the same path twice.
	// Maps to CWMP fault 9003.
	FailureDuplicatePath
)

// SetBatchError reports which SetBatch entry failed and why. The
// handler reads Code to pick the wire-side CWMP fault code. Path/Code/
// Err describe the FIRST failure; All carries every pre-flight failure
// in input order (always at least one entry) so the CWMP handler can
// render the spec's per-parameter SetParameterValuesFault list.
type SetBatchError struct {
	Path string
	Code SetBatchFailure
	Err  error
	All  []EntryFault
}

// EntryFault is one per-entry pre-flight failure inside a SetBatch.
type EntryFault struct {
	Path string
	Code SetBatchFailure
	Err  error
}

// Error implements the error interface.
func (e *SetBatchError) Error() string {
	if e == nil {
		return "<nil paramtree.SetBatchError>"
	}
	return fmt.Sprintf("paramtree.SetBatch: %s at %q: %v", e.codeString(), e.Path, e.Err)
}

// Unwrap exposes the underlying cause for errors.Is / errors.As.
func (e *SetBatchError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Err
}

func (e *SetBatchError) codeString() string {
	switch e.Code {
	case FailureNotFound:
		return "not found"
	case FailureNotWritable:
		return "not writable"
	case FailureTypeMismatch:
		return "type mismatch"
	case FailureInvalidValue:
		return "invalid value"
	case FailureDuplicatePath:
		return "duplicate path"
	}
	return "unknown"
}

// SetBatch atomically applies all setters: every entry is validated
// (existence, writability, type stability, value validation, and
// uniqueness within the batch) before any mutation. If any entry fails
// validation, no leaf is mutated and the offending path's
// *SetBatchError is returned.
//
// On success returns one BatchResult per input setter in input order.
// An empty input is a valid no-op and returns (nil, nil).
//
// Concurrency: holds the write lock for the duration of the batch.
func (t *Tree) SetBatch(setters []Setter) ([]BatchResult, error) {
	if len(setters) == 0 {
		return nil, nil
	}

	t.mu.Lock()
	defer t.mu.Unlock()

	// Pre-flight: detect duplicates, resolve every path, check
	// writability + type stability + value validity. No mutation.
	type prepared struct {
		node *Node
		old  Value
		new  Value
	}
	resolved := make([]prepared, 0, len(setters))
	seen := make(map[string]struct{}, len(setters))

	// Pre-flight collects EVERY failing entry rather than aborting on
	// the first: TR-069 A.5.1 requires the SPV fault response to list
	// each failing parameter with its own code, and a real CPE
	// validates the whole batch before applying any of it.
	var faults []EntryFault
	addFault := func(path string, code SetBatchFailure, err error) {
		faults = append(faults, EntryFault{Path: path, Code: code, Err: err})
	}

	for _, s := range setters {
		segments, err := parsePath(s.Path)
		if err != nil {
			addFault(s.Path, FailureNotFound, err)
			continue
		}
		canonical := joinPath(segments)
		if _, dup := seen[canonical]; dup {
			addFault(s.Path, FailureDuplicatePath, fmt.Errorf("path %q appears twice in batch", s.Path))
			continue
		}
		seen[canonical] = struct{}{}

		n, err := t.lookup(segments)
		if err != nil {
			addFault(s.Path, FailureNotFound, err)
			continue
		}
		if !n.isLeaf() {
			addFault(s.Path, FailureNotFound, fmt.Errorf("path %q is an interior node, not a leaf", s.Path))
			continue
		}
		if !n.leaf.Writable {
			addFault(s.Path, FailureNotWritable, fmt.Errorf("path %q is read-only", s.Path))
			continue
		}
		if n.leaf.Type != s.Value.Type {
			addFault(s.Path, FailureTypeMismatch, fmt.Errorf("type mismatch at %q: have %s, got %s", s.Path, n.leaf.Type, s.Value.Type))
			continue
		}
		if err := Validate(s.Value.Type, s.Value.Raw); err != nil {
			addFault(s.Path, FailureInvalidValue, err)
			continue
		}
		resolved = append(resolved, prepared{node: n, old: *n.leaf, new: s.Value})
	}

	if len(faults) > 0 {
		first := faults[0]
		return nil, &SetBatchError{Path: first.Path, Code: first.Code, Err: first.Err, All: faults}
	}

	// No faults means resolved is 1:1 with setters (every entry either
	// faulted and returned above, or resolved), so indexing setters[i]
	// below is aligned.
	results := make([]BatchResult, len(resolved))
	for i, r := range resolved {
		*r.node.leaf = r.new
		results[i] = BatchResult{
			Path:     setters[i].Path,
			OldValue: r.old,
			NewValue: r.new,
			Changed:  r.old.Raw != r.new.Raw,
		}
	}
	return results, nil
}

// Names returns parameter paths visible under prefix.
//
// partial == false: exact match. If prefix names a leaf, returns
// {prefix}; if prefix names an interior node, returns the immediate
// children only (matches TR-069 GetParameterNames with NextLevel=true).
//
// partial == true: prefix expansion. Returns every leaf path that
// starts with prefix, in lexicographic order (matches TR-069
// GetParameterNames with NextLevel=false on a partial path).
//
// prefix may end in "." to denote an interior node; with or without
// the trailing dot, the lookup is the same.
func (t *Tree) Names(prefix string, partial bool) ([]string, error) {
	segments, err := parsePath(prefix)
	if err != nil {
		return nil, err
	}
	t.mu.RLock()
	defer t.mu.RUnlock()

	n, err := t.lookup(segments)
	if err != nil {
		return nil, cpeerr.Wrap("paramtree.Names", cpeerr.KindNotFound, err)
	}

	if partial {
		var out []string
		collectLeaves(n, segments, &out)
		sort.Strings(out)
		return out, nil
	}

	// Exact match: leaf returns itself; interior returns immediate children.
	if n.isLeaf() {
		return []string{joinPath(segments)}, nil
	}
	out := make([]string, 0, len(n.children))
	for seg := range n.children {
		out = append(out, joinPath(append(append([]string{}, segments...), seg)))
	}
	sort.Strings(out)
	return out, nil
}

// Walk visits every leaf under prefix in lexicographic order, calling
// fn for each. depth caps recursion: depth == 0 means unlimited;
// depth == 1 means immediate children only. fn returning a non-nil
// error halts the walk and returns that error.
//
// fn runs while Walk holds the read lock; do not re-enter the same
// Tree from fn or you will deadlock.
// ChildInfo describes one immediate child of an interior node.
// For interior children, Name ends with "." and Writable is false;
// for leaves, Name does not end with "." and Writable reflects the
// leaf's flag.
type ChildInfo struct {
	Name     string
	Writable bool
}

// Children returns the immediate children of an interior node at
// prefix. Useful for CWMP GetParameterNames with NextLevel=true.
// Results are lexicographically sorted.
//
// Returns cpeerr.KindNotFound if prefix doesn't resolve or names a
// leaf (leaves have no children).
func (t *Tree) Children(prefix string) ([]ChildInfo, error) {
	segments, err := parsePath(prefix)
	if err != nil {
		return nil, err
	}
	t.mu.RLock()
	defer t.mu.RUnlock()

	n, err := t.lookup(segments)
	if err != nil {
		return nil, cpeerr.Wrap("paramtree.Children", cpeerr.KindNotFound, err)
	}
	if n.isLeaf() {
		return nil, cpeerr.Wrap("paramtree.Children", cpeerr.KindNotFound,
			fmt.Errorf("path %q is a leaf, has no children", prefix))
	}

	out := make([]ChildInfo, 0, len(n.children))
	for _, seg := range sortedKeys(n.children) {
		child := n.children[seg]
		fullPath := joinPath(append(append([]string{}, segments...), seg))
		if child.isLeaf() {
			out = append(out, ChildInfo{Name: fullPath, Writable: child.leaf.Writable})
		} else {
			out = append(out, ChildInfo{Name: fullPath + ".", Writable: false})
		}
	}
	return out, nil
}

func (t *Tree) Walk(prefix string, depth int, fn func(path string, v Value) error) error {
	segments, err := parsePath(prefix)
	if err != nil {
		return err
	}
	t.mu.RLock()
	defer t.mu.RUnlock()

	n, err := t.lookup(segments)
	if err != nil {
		return cpeerr.Wrap("paramtree.Walk", cpeerr.KindNotFound, err)
	}
	return walk(n, segments, depth, fn)
}

// lookup walks the tree by segments. Returns a typed not-found cause
// (no cpeerr wrapping; the caller wraps with the right Op).
func (t *Tree) lookup(segments []string) (*Node, error) {
	n := t.root
	for i, seg := range segments {
		if n.isLeaf() {
			return nil, fmt.Errorf("path %q traverses a leaf at %q",
				joinPath(segments), joinPath(segments[:i]))
		}
		child, ok := n.children[seg]
		if !ok {
			return nil, fmt.Errorf("path %q not found", joinPath(segments))
		}
		n = child
	}
	return n, nil
}

// collectLeaves appends every leaf path under n to out. prefix is the
// path of n itself (used to render leaf paths).
func collectLeaves(n *Node, prefix []string, out *[]string) {
	if n.isLeaf() {
		*out = append(*out, joinPath(prefix))
		return
	}
	for seg, child := range n.children {
		next := append(append([]string{}, prefix...), seg)
		collectLeaves(child, next, out)
	}
}

// walk implements Walk's recursion. depth==0 means unlimited.
func walk(n *Node, prefix []string, depth int, fn func(path string, v Value) error) error {
	if n.isLeaf() {
		return fn(joinPath(prefix), *n.leaf)
	}
	if depth == 1 {
		// emit only immediate-child leaves
		segs := sortedKeys(n.children)
		for _, seg := range segs {
			child := n.children[seg]
			if child.isLeaf() {
				if err := fn(joinPath(append(append([]string{}, prefix...), seg)), *child.leaf); err != nil {
					return err
				}
			}
		}
		return nil
	}
	next := depth
	if depth > 1 {
		next = depth - 1
	}
	segs := sortedKeys(n.children)
	for _, seg := range segs {
		child := n.children[seg]
		if err := walk(child, append(append([]string{}, prefix...), seg), next, fn); err != nil {
			return err
		}
	}
	return nil
}

func sortedKeys(m map[string]*Node) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// AddTable declares parentPath as a variable-arity table whose
// instances follow template. AddObject calls on parentPath will clone
// template into a fresh instance. AddTable is intended for tree
// construction (test helpers, profile loader) and is callable before
// any AddObject on the same path.
func (t *Tree) AddTable(parentPath string, template *Node) error {
	segments, err := parsePath(parentPath)
	if err != nil {
		return err
	}
	if len(segments) == 0 {
		return cpeerr.Wrap("paramtree.AddTable", cpeerr.KindInvalidArgument,
			fmt.Errorf("cannot mark root as a table"))
	}
	if template == nil {
		return cpeerr.Wrap("paramtree.AddTable", cpeerr.KindInvalidArgument,
			fmt.Errorf("template is nil"))
	}

	t.mu.Lock()
	defer t.mu.Unlock()

	// Mount an empty branch for the table parent if it doesn't exist.
	parent := t.root
	for _, seg := range segments {
		if parent.isLeaf() {
			return cpeerr.Wrap("paramtree.AddTable", cpeerr.KindInvalidArgument,
				fmt.Errorf("path %q traverses a leaf", parentPath))
		}
		child, ok := parent.children[seg]
		if !ok {
			child = NewBranch()
			parent.children[seg] = child
		}
		parent = child
	}
	if parent.isLeaf() {
		return cpeerr.Wrap("paramtree.AddTable", cpeerr.KindInvalidArgument,
			fmt.Errorf("path %q is a leaf", parentPath))
	}
	if parent.table != nil {
		return cpeerr.Wrap("paramtree.AddTable", cpeerr.KindInvalidArgument,
			fmt.Errorf("path %q is already a table", parentPath))
	}
	parent.table = &tableMeta{template: template}
	return nil
}

// AddObject creates a new instance under the table at parentPath and
// returns its instance number. parentPath must have been declared as
// a table via AddTable. Instance numbers start at 1 and are assigned
// as the smallest unused positive integer.
func (t *Tree) AddObject(parentPath string) (int, error) {
	// Notify outside the lock; see Tree.Observe.
	var created string
	var counter *Change
	defer func() {
		if created != "" {
			t.notify(Change{Path: created, Kind: ChangeObjectCreated})
		}
		if counter != nil {
			t.notify(*counter)
		}
	}()

	segments, err := parsePath(parentPath)
	if err != nil {
		return 0, err
	}
	t.mu.Lock()
	defer t.mu.Unlock()

	n, err := t.lookup(segments)
	if err != nil {
		return 0, cpeerr.Wrap("paramtree.AddObject", cpeerr.KindNotFound, err)
	}
	if n.table == nil {
		return 0, cpeerr.Wrap("paramtree.AddObject", cpeerr.KindInvalidArgument,
			fmt.Errorf("path %q is not a table", parentPath))
	}

	used := make(map[int]struct{}, len(n.children))
	for k := range n.children {
		if i, err := strconv.Atoi(k); err == nil && i > 0 {
			used[i] = struct{}{}
		}
	}
	instance := 1
	for {
		if _, taken := used[instance]; !taken {
			break
		}
		instance++
	}
	n.children[strconv.Itoa(instance)] = n.table.template.clone()
	counter = t.syncEntryCount(segments, n)
	if t.hasObservers() {
		created = strings.TrimSuffix(parentPath, ".") + "." + strconv.Itoa(instance) + "."
	}
	return instance, nil
}

// syncEntryCount rewrites the sibling <Table>NumberOfEntries leaf after
// an instance mutation, when the profile declares one. Both TR-098 and
// TR-181 name the counter this way next to the table, and it is how a
// real CPE advertises table size, so the simulator keeps it honest
// rather than leaving the boot-time value frozen. Called with the tree
// lock held; the returned change, if any, is notified by the caller
// after the lock is released.
func (t *Tree) syncEntryCount(tableSegs []string, table *Node) *Change {
	if len(tableSegs) == 0 {
		return nil
	}
	parent, err := t.lookup(tableSegs[:len(tableSegs)-1])
	if err != nil {
		return nil
	}
	name := tableSegs[len(tableSegs)-1] + "NumberOfEntries"
	c, ok := parent.children[name]
	if !ok || !c.isLeaf() {
		return nil
	}
	count := 0
	for k := range table.children {
		if i, err := strconv.Atoi(k); err == nil && i > 0 {
			count++
		}
	}
	raw := strconv.Itoa(count)
	if c.leaf.Raw == raw {
		return nil
	}
	old := *c.leaf
	c.leaf.Raw = raw
	if !t.hasObservers() {
		return nil
	}
	path := joinPath(tableSegs[:len(tableSegs)-1])
	if path != "" {
		path += "."
	}
	return &Change{Path: path + name, Old: old, New: *c.leaf, Kind: ChangeValue}
}

// DeleteObject removes the instance sub-tree at path. path must name
// an instance under a table (the last segment must be a positive
// integer string). Returns cpeerr.KindInvalidArgument if path is not
// a table instance, or cpeerr.KindNotFound if the instance does not
// exist.
func (t *Tree) DeleteObject(path string) error {
	var deleted string
	var counter *Change
	defer func() {
		if deleted != "" {
			t.notify(Change{Path: deleted, Kind: ChangeObjectDeleted})
		}
		if counter != nil {
			t.notify(*counter)
		}
	}()

	segments, err := parsePath(path)
	if err != nil {
		return err
	}
	if len(segments) == 0 {
		return cpeerr.Wrap("paramtree.DeleteObject", cpeerr.KindInvalidArgument,
			fmt.Errorf("cannot delete root"))
	}

	t.mu.Lock()
	defer t.mu.Unlock()

	last := segments[len(segments)-1]
	instance, err := strconv.Atoi(last)
	if err != nil || instance <= 0 {
		return cpeerr.Wrap("paramtree.DeleteObject", cpeerr.KindInvalidArgument,
			fmt.Errorf("path %q does not name a table instance (last segment must be a positive integer)", path))
	}

	parentSegs := segments[:len(segments)-1]
	parent, err := t.lookup(parentSegs)
	if err != nil {
		return cpeerr.Wrap("paramtree.DeleteObject", cpeerr.KindNotFound, err)
	}
	if parent.table == nil {
		return cpeerr.Wrap("paramtree.DeleteObject", cpeerr.KindInvalidArgument,
			fmt.Errorf("path %q does not name a table instance (parent is not a table)", path))
	}
	if _, exists := parent.children[last]; !exists {
		return cpeerr.Wrap("paramtree.DeleteObject", cpeerr.KindNotFound,
			fmt.Errorf("instance %s of %s not found", last, joinPath(parentSegs)))
	}
	delete(parent.children, last)
	counter = t.syncEntryCount(parentSegs, parent)
	if t.hasObservers() {
		deleted = path
	}
	return nil
}
