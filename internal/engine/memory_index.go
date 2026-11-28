package engine

import (
	"cmp"
	"iter"
	"slices"
	"strings"
)

// 压缩前缀树按键寻址。分叉最多 256 路，路径长度受键长限制；写一个键只需
// 复制路径上的节点。相比复制整张 map，发布新快照的成本与历史聚合数无关。
type indexNode struct {
	prefix   string
	row      *memoryRow
	children []*indexNode // 按分叉字节升序，固定宽度的序号键可直接有序遍历
	owner    *writeMark
}

// 非零大小保证每个写事务（包括保存点）有独立地址；标记不引用事务或旧根。
type writeMark byte

type memoryRow struct {
	inputs      *memoryInputs
	origin      string   // 在办工作的不可变起源事件，用于刷新按起源对象的反查
	subscribers []string // Thread 的反查索引键，避免更新索引时再次解码聚合
	data        []byte
	lookup      string // 辅助索引的关联键；更新聚合时用它移除旧项
	order       string // 首次插入的位置，仅活跃集需要
}

func (n *indexNode) edge(b byte) (int, bool) {
	return slices.BinarySearchFunc(n.children, b, func(child *indexNode, b byte) int {
		return cmp.Compare(child.prefix[len(n.prefix)], b)
	})
}

// prefixTree 也供关联反查使用；前缀可能恰好落在一段压缩路径中间。
func (n *indexNode) prefixTree(key string) *indexNode {
	for n != nil && len(key) > len(n.prefix) {
		if !strings.HasPrefix(key, n.prefix) {
			return nil
		}
		i, ok := n.edge(key[len(n.prefix)])
		if !ok {
			return nil
		}
		n = n.children[i]
	}
	if n != nil && strings.HasPrefix(n.prefix, key) {
		return n
	}
	return nil
}

func (n *indexNode) get(key string) *memoryRow {
	if n = n.prefixTree(key); n != nil && n.prefix == key {
		return n.row
	}
	return nil
}

func (n *indexNode) writable(mark *writeMark) *indexNode {
	if n.owner == mark {
		return n
	}
	next := *n
	next.owner = mark
	next.children = slices.Clone(n.children)
	return &next
}

// set 返回新根；nil 行删除该键。单子节点的空分叉直接折叠，使活跃索引
// 的遍历最多经过约两倍于结果数的节点，包括从大活跃集缩小后的情况。
func (n *indexNode) set(mark *writeMark, key string, row *memoryRow) *indexNode {
	if n == nil {
		if row == nil {
			return nil
		}
		return &indexNode{prefix: key, row: row, owner: mark}
	}
	if !strings.HasPrefix(key, n.prefix) {
		if row == nil {
			return n
		}
		i := 0
		for i < len(key) && i < len(n.prefix) && key[i] == n.prefix[i] {
			i++
		}
		n = &indexNode{prefix: key[:i], children: []*indexNode{n}, owner: mark}
	}
	n = n.writable(mark)
	if key == n.prefix {
		n.row = row
	} else {
		i, found := n.edge(key[len(n.prefix)])
		if !found {
			if row != nil {
				n.children = slices.Insert(n.children, i, &indexNode{prefix: key, row: row, owner: mark})
			}
		} else if child := n.children[i].set(mark, key, row); child != nil {
			n.children[i] = child
		} else {
			n.children = slices.Delete(n.children, i, i+1)
		}
	}
	if n.row == nil {
		switch len(n.children) {
		case 0:
			return nil
		case 1:
			return n.children[0]
		}
	}
	return n
}

func (n *indexNode) walk(visit func(*memoryRow) error) error {
	if n == nil {
		return nil
	}
	if n.row != nil {
		if err := visit(n.row); err != nil {
			return err
		}
	}
	for _, child := range n.children {
		if err := child.walk(visit); err != nil {
			return err
		}
	}
	return nil
}

// 倒序遍历支持读足即停；压缩树没有空分叉，访问量由返回数和跳过数决定。
func (n *indexNode) reverseRows() iter.Seq[*memoryRow] {
	return func(yield func(*memoryRow) bool) { n.walkReverse(yield) }
}

func (n *indexNode) walkReverse(yield func(*memoryRow) bool) bool {
	if n == nil {
		return true
	}
	for i := len(n.children) - 1; i >= 0; i-- {
		if !n.children[i].walkReverse(yield) {
			return false
		}
	}
	return n.row == nil || yield(n.row)
}
