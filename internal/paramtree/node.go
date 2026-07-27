package paramtree

import (
	"fmt"

	"github.com/ispx-limited/cpe-labs/internal/cpeerr"
)

// Node is the construction-time representation of a sub-tree. Once a
// Node is attached to a Tree (via Mount or AddTable), the Tree owns it;
// callers must not mutate the Node afterwards.
type Node struct {
	children map[string]*Node
	leaf     *Value
	attrs    *Attributes
	table    *tableMeta
}

// tableMeta carries the template Tree.AddObject clones for new
// instances. Set by AddTable, nil for non-table interior nodes.
type tableMeta struct {
	template *Node
}

// NewBranch returns an empty interior node ready for child attachment.
func NewBranch() *Node {
	return &Node{children: make(map[string]*Node)}
}

// NewLeaf returns a leaf node holding the given Value.
func NewLeaf(v Value) *Node {
	return &Node{leaf: &v}
}

// Attach binds child as the named segment under n. Reports an error
// if the segment is already taken or if n is a leaf.
func (n *Node) Attach(segment string, child *Node) error {
	if n.leaf != nil {
		return cpeerr.Wrap("paramtree.Node.Attach", cpeerr.KindInvalidArgument,
			fmt.Errorf("cannot attach %q under a leaf node", segment))
	}
	if n.children == nil {
		n.children = make(map[string]*Node)
	}
	if _, exists := n.children[segment]; exists {
		return cpeerr.Wrap("paramtree.Node.Attach", cpeerr.KindInvalidArgument,
			fmt.Errorf("segment %q already attached", segment))
	}
	n.children[segment] = child
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
		cp.children = make(map[string]*Node, len(n.children))
		for k, c := range n.children {
			cp.children[k] = c.clone()
		}
	}
	if n.table != nil {
		cp.table = &tableMeta{template: n.table.template.clone()}
	}
	return cp
}
