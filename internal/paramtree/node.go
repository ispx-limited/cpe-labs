package paramtree

import (
	"fmt"
	"sort"

	"github.com/ispx-limited/cpe-labs/internal/cpeerr"
)

// Node is the construction-time representation of a sub-tree. Once a
// Node is attached to a Tree (via Mount or AddTable), the Tree owns it;
// callers must not mutate the Node afterwards.
type Node struct {
	children kids
	leaf     *Value
	attrs    *Attributes
	table    *tableMeta

	// owner is the generation of the one Tree allowed to write this
	// node in place. Every other tree that reaches it copies it first;
	// see Tree.own.
	owner uint64
}

// kids is a node's children, sorted by name. A slice rather than a map:
// a tree copies a node's children whenever it takes ownership of the
// node (Tree.own), and a fleet does that for every branch above every
// leaf a CPE writes. Copying a slice is one allocation and a memmove;
// copying a map rebuilds its hash buckets and costs about twice the
// memory, which made those copies most of what a simulated CPE held.
type kids []kid

type kid struct {
	name string
	node *Node
}

func (k kids) find(name string) (int, bool) {
	i := sort.Search(len(k), func(i int) bool { return k[i].name >= name })
	return i, i < len(k) && k[i].name == name
}

func (k kids) get(name string) (*Node, bool) {
	if i, ok := k.find(name); ok {
		return k[i].node, true
	}
	return nil, false
}

func (k *kids) set(name string, n *Node) {
	i, ok := k.find(name)
	if ok {
		(*k)[i].node = n
		return
	}
	*k = append(*k, kid{})
	copy((*k)[i+1:], (*k)[i:])
	(*k)[i] = kid{name: name, node: n}
}

func (k *kids) del(name string) {
	if i, ok := k.find(name); ok {
		*k = append((*k)[:i], (*k)[i+1:]...)
	}
}

// names returns the children's names in order.
func (k kids) names() []string {
	out := make([]string, len(k))
	for i, c := range k {
		out[i] = c.name
	}
	return out
}

// tableMeta carries the template Tree.AddObject clones for new
// instances. Set by AddTable, nil for non-table interior nodes.
type tableMeta struct {
	template *Node
}

// NewBranch returns an empty interior node ready for child attachment.
func NewBranch() *Node {
	return &Node{children: kids{}}
}

// NewLeaf returns a leaf node holding the given Value.
func NewLeaf(v Value) *Node {
	return &Node{leaf: &v}
}

// NewTable returns an interior node declared as a multi-instance table
// whose instances clone template. AddTable declares a table by path on a
// mounted tree; a table nested inside another table's template has no
// path until AddObject clones the template, so it has to be declared on
// the node itself. Device.BulkData.Profile.{i}.Parameter.{i}. is the
// case: every Profile instance carries its own Parameter table.
func NewTable(template *Node) *Node {
	return &Node{children: kids{}, table: &tableMeta{template: template}}
}

// Attach binds child as the named segment under n. Reports an error
// if the segment is already taken or if n is a leaf.
func (n *Node) Attach(segment string, child *Node) error {
	if n.leaf != nil {
		return cpeerr.Wrap("paramtree.Node.Attach", cpeerr.KindInvalidArgument,
			fmt.Errorf("cannot attach %q under a leaf node", segment))
	}
	if _, exists := n.children.get(segment); exists {
		return cpeerr.Wrap("paramtree.Node.Attach", cpeerr.KindInvalidArgument,
			fmt.Errorf("segment %q already attached", segment))
	}
	n.children.set(segment, child)
	return nil
}

// isLeaf reports whether n is a leaf node (carries a Value).
func (n *Node) isLeaf() bool {
	return n.leaf != nil
}

// clone deep-copies n. Used by AddObject to materialize a fresh
// instance from a table template.
func (n *Node) clone() *Node {
	if n == nil {
		return nil
	}
	cp := &Node{}
	if n.leaf != nil {
		v := *n.leaf
		cp.leaf = &v
	}
	if n.attrs != nil {
		a := *n.attrs
		if n.attrs.AccessList != nil {
			a.AccessList = append([]string(nil), n.attrs.AccessList...)
		}
		cp.attrs = &a
	}
	if n.children != nil {
		cp.children = make(kids, len(n.children))
		for i, c := range n.children {
			cp.children[i] = kid{name: c.name, node: c.node.clone()}
		}
	}
	if n.table != nil {
		cp.table = &tableMeta{template: n.table.template.clone()}
	}
	return cp
}
