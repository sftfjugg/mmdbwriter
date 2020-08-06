// Package mmdbwriter provides the tools to create and write MaxMind DB
// files.
package mmdbwriter

import (
	"bufio"
	"io"
	"net"
	"time"

	"github.com/pkg/errors"
)

var (
	metadataStartMarker  = []byte("\xAB\xCD\xEFMaxMind.com")
	dataSectionSeparator = []byte{0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0}
)

// Options holds configuration parameters for the writer
type Options struct {
	// BuildEpoch is the database build timestamp as a Unix epoch value. It
	// defaults to the epoch of when New was called.
	BuildEpoch int64

	// DatabaseType is a string that indicates the structure of each data record
	// associated with an IP address. The actual definition of these structures
	// is left up to the database creator.
	DatabaseType string

	// Description is a map where the key is a language code and the value is
	// the description of the database in that language.
	Description map[string]string

	// IPVersion indicates whether an IPv4 or IPv6 database should be built. An
	// IPv6 database supports both IPv4 and IPv6 lookups. The default value is
	// "6" for IPv6.
	IPVersion int

	// Languages is a slice of strings, each of which is a locale code. A given
	// record may contain data items that have been localized to some or all of
	// these locales. Records should not contain localized data for locales not
	// included in this slice.
	Languages []string

	// RecordSize indicates the number of bits in a record in the search tree.
	// The supported values are 24, 28, and 32. A smaller size will result in a
	// smaller database, but it will limit the maximum size of the database.
	// The default is 28.
	RecordSize int
}

// Tree represents an MaxMind DB search tree.
type Tree struct {
	buildEpoch   int64
	databaseType string
	description  map[string]string
	ipVersion    int
	languages    []string
	recordSize   int
	root         *node
	treeDepth    int
	// This is set when the tree is finalized
	nodeCount int
}

// New creates a new Tree.
func New(opts Options) (*Tree, error) {
	tree := &Tree{
		buildEpoch:   time.Now().Unix(),
		databaseType: opts.DatabaseType,
		description:  map[string]string{},
		ipVersion:    6,
		recordSize:   28,
		root:         &node{},
	}

	if opts.BuildEpoch != 0 {
		tree.buildEpoch = opts.BuildEpoch
	}

	if opts.Description != nil {
		tree.description = opts.Description
	}

	if opts.IPVersion != 0 {
		tree.ipVersion = opts.IPVersion
	}

	if opts.Languages != nil {
		tree.languages = opts.Languages
	}

	if opts.RecordSize != 0 {
		tree.recordSize = opts.RecordSize
	}

	switch tree.ipVersion {
	case 6:
		tree.treeDepth = 128
	case 4:
		tree.treeDepth = 32
	default:
		return nil, errors.Errorf("unsupported IPVersion: %d", tree.ipVersion)
	}

	return tree, nil
}

// Insert a data value into the tree.
func (t *Tree) Insert(
	network *net.IPNet,
	// TODO - We current only support inserting dataType. In the future, we
	// should support arbitrary tagged structs
	value DataType,
) error {
	// We set this to 0 so that the tree must be finalized again.
	t.nodeCount = 0

	prefixLen, _ := network.Mask.Size()

	if prefixLen == 0 {
		// It isn't possible to do this as there isn't a record for the root node.
		// If we wanted to support this, we would have to divide it into two /1
		// insertions, but there isn't a reason to bother supporting it.
		return errors.New("cannot insert a value into the root node of the tree")
	}

	ip := network.IP
	if t.treeDepth == 128 && len(ip) == 4 {
		ip = ipV4ToV6(ip)
		prefixLen += 96
	}

	t.root.insert(ip, prefixLen, 0, value)
	return nil
}

// Get the value for the given IP address from the tree.
func (t *Tree) Get(ip net.IP) (*net.IPNet, *DataType) {
	lookupIP := ip

	if t.treeDepth == 128 {
		// We use To4() here as Go will parse an IPv4 address to a 16 byte
		// IPv6-mapped IPv4 address, e.g.:
		//
		// len(net.ParseIP("1.1.1.1")) == 16
		//
		// The parsed address above is equal to ::ffff:1.1.1.1. However,
		// the MaxMind DB format has the record for 1.1.1.1 at ::1.1.1.1.
		if ipv4 := ip.To4(); ipv4 != nil {
			lookupIP = ipV4ToV6(ipv4)
		}
	}

	prefixLen, value := t.root.get(lookupIP, 0)

	// This is so that if you look up an IPv4 address in a database that has
	// an IPv4 subtree, you will get back an IPv4 network. This matches what
	// github.com/oschwald/maxminddb-golang does.
	if prefixLen >= 96 && len(ip) == 4 {
		prefixLen -= 96
	}

	mask := net.CIDRMask(prefixLen, t.treeDepth)

	return &net.IPNet{
		IP:   ip.Mask(mask),
		Mask: mask,
	}, value
}

// Finalize prepares the tree for writing. It is not threadsafe.
func (t *Tree) Finalize() {
	t.nodeCount = t.root.finalize(0)
}

// WriteTo writes the tree to the provided Writer.
func (t *Tree) WriteTo(w io.Writer) (int64, error) {
	if t.nodeCount == 0 {
		return 0, errors.New("the Tree is not finalized; run Finalize() before writing")
	}

	buf := bufio.NewWriter(w)

	// We create this here so that we don't have to allocate millions of these. This
	// may no longer make sense now that we are using a bufio.Writer anyway, which has
	// WriteByte, but we should probably do some testing.
	recordBuf := make([]byte, 2*t.recordSize/8)

	dataWriter := newDataWriter()

	nodeCount, numBytes, err := t.writeNode(buf, t.root, dataWriter, recordBuf)
	if err != nil {
		_ = buf.Flush()
		return numBytes, err
	}
	if nodeCount != t.nodeCount {
		_ = buf.Flush()
		// This should only happen if there is a programming bug
		// in this library.
		return numBytes, errors.Errorf(
			"number of nodes written (%d) doesn't match number expected (%d)",
			nodeCount,
			t.nodeCount,
		)
	}

	nb, err := buf.Write(dataSectionSeparator)
	numBytes += int64(nb)
	if err != nil {
		_ = buf.Flush()
		return numBytes, errors.Wrap(err, "error writing data section separator")
	}

	nb64, err := dataWriter.buf.WriteTo(buf)
	numBytes += nb64
	if err != nil {
		_ = buf.Flush()
		return numBytes, err
	}

	nb, err = buf.Write(metadataStartMarker)
	numBytes += int64(nb)
	if err != nil {
		_ = buf.Flush()
		return numBytes, errors.Wrap(err, "error writing metadata start marker")
	}

	nb64, err = t.writeMetadata(buf)
	numBytes += nb64
	if err != nil {
		_ = buf.Flush()
		return numBytes, errors.Wrap(err, "error writing metadata")
	}

	err = buf.Flush()
	if err != nil {
		return numBytes, errors.Wrap(err, "error flushing buffer to writer")
	}

	return numBytes, err
}

func (t *Tree) writeNode(
	w io.Writer,
	n *node,
	dataWriter *dataWriter,
	recordBuf []byte,
) (int, int64, error) {
	if n.isLeaf() {
		return 0, 0, nil
	}

	err := t.copyRecord(recordBuf, n.children, dataWriter)
	if err != nil {
		return 0, 0, err
	}

	numBytes := int64(0)
	nb, err := w.Write(recordBuf)
	numBytes += int64(nb)
	nodesWritten := 1
	if err != nil {
		return nodesWritten, numBytes, errors.Wrap(err, "error writing node")
	}

	leftNodes, leftNumBytes, err := t.writeNode(
		w,
		n.children[0],
		dataWriter,
		recordBuf,
	)
	nodesWritten += leftNodes
	numBytes += leftNumBytes
	if err != nil {
		return nodesWritten, numBytes, err
	}

	rightNodes, rightNumBytes, err := t.writeNode(
		w,
		n.children[1],
		dataWriter,
		recordBuf,
	)
	nodesWritten += rightNodes
	numBytes += rightNumBytes
	return nodesWritten, numBytes, err
}

func (t *Tree) recordValueForNode(
	n *node,
	dataWriter *dataWriter,
) (int, error) {
	if n.isLeaf() {
		if n.value != nil {
			offset, err := dataWriter.write(*n.value)
			return t.nodeCount + len(dataSectionSeparator) + offset, err
		}
		return t.nodeCount, nil
	}
	return n.nodeNum, nil
}

func (t *Tree) copyRecord(buf []byte, children [2]*node, dataWriter *dataWriter) error {
	left, err := t.recordValueForNode(children[0], dataWriter)
	if err != nil {
		return err
	}
	right, err := t.recordValueForNode(children[1], dataWriter)
	if err != nil {
		return err
	}

	// XXX check max size

	switch t.recordSize {
	case 24:
		buf[0] = byte((left >> 16) & 0xFF)
		buf[1] = byte((left >> 8) & 0xFF)
		buf[2] = byte(left & 0xFF)
		buf[3] = byte((right >> 16) & 0xFF)
		buf[4] = byte((right >> 8) & 0xFF)
		buf[5] = byte(right & 0xFF)
	case 28:
		buf[0] = byte((left >> 16) & 0xFF)
		buf[1] = byte((left >> 8) & 0xFF)
		buf[2] = byte(left & 0xFF)
		buf[3] = byte((((left >> 24) & 0x0F) << 4) | (right >> 24 & 0x0F))
		buf[4] = byte((right >> 16) & 0xFF)
		buf[5] = byte((right >> 8) & 0xFF)
		buf[6] = byte(right & 0xFF)
	case 32:
		buf[0] = byte((left >> 24) & 0xFF)
		buf[1] = byte((left >> 16) & 0xFF)
		buf[2] = byte((left >> 8) & 0xFF)
		buf[3] = byte(left & 0xFF)
		buf[4] = byte((right >> 24) & 0xFF)
		buf[5] = byte((right >> 16) & 0xFF)
		buf[6] = byte((right >> 8) & 0xFF)
		buf[7] = byte(right & 0xFF)
	default:
		return errors.Errorf("unsupported record size of %d", t.recordSize)
	}
	return nil
}

var v4Prefix = net.IP{0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0}

func ipV4ToV6(ip net.IP) net.IP {
	return append(v4Prefix, ip...)
}

func (t *Tree) writeMetadata(w *bufio.Writer) (int64, error) {
	description := Map{}
	for k, v := range t.description {
		description[String(k)] = String(v)
	}

	languages := Slice{}
	for _, v := range t.languages {
		languages = append(languages, String(v))
	}
	metadata := Map{
		"binary_format_major_version": Uint16(2),
		"binary_format_minor_version": Uint16(0),
		"build_epoch":                 Uint64(t.buildEpoch),
		"database_type":               String(t.databaseType),
		"description":                 description,
		"ip_version":                  Uint16(t.ipVersion),
		"languages":                   languages,
		"node_count":                  Uint32(t.nodeCount),
		"record_size":                 Uint16(t.recordSize),
	}
	return metadata.writeTo(w)
}