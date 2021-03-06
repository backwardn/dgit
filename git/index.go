package git

import (
	"crypto/sha1"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"os"
	"sort"
	"strings"
)

var InvalidIndex error = errors.New("Invalid index")

// index file is defined as Network byte Order (Big Endian)

// 12 byte header:
// 4 bytes: D I R C (stands for "Dir cache")
// 4-byte version number (can be 2, 3 or 4)
// 32bit number of index entries
type fixedGitIndex struct {
	Signature          [4]byte // 4
	Version            uint32  // 8
	NumberIndexEntries uint32  // 12
}

type Index struct {
	fixedGitIndex // 12
	Objects       []*IndexEntry
}

type V3IndexExtensions struct {
	Flags uint16
}

type IndexEntry struct {
	FixedIndexEntry
	*V3IndexExtensions
	PathName IndexPath
}

func (ie IndexEntry) Stage() Stage {
	return Stage((ie.FixedIndexEntry.Flags >> 12) & 0x3)
}

func (ie IndexEntry) SkipWorktree() bool {
	if ie.ExtendedFlag() == false || ie.V3IndexExtensions == nil {
		return false
	}
	return (ie.V3IndexExtensions.Flags>>14)&0x1 == 1
}
func (ie *IndexEntry) SetSkipWorktree(value bool) {
	if value {
		// If it's being set, we need to set the extended
		// flag. If it's not being set, we don't care, but
		// we don't change it in case intend-to-add is set.
		ie.FixedIndexEntry.SetExtendedFlag(true)
	}

	if ie.V3IndexExtensions == nil {
		ie.V3IndexExtensions = &V3IndexExtensions{}
	}
	if value == true {
		ie.V3IndexExtensions.Flags |= (0x1 << 14)
	} else {
		ie.V3IndexExtensions.Flags &^= (0x1 << 14)
	}
}

func NewIndex() *Index {
	return &Index{
		fixedGitIndex: fixedGitIndex{
			Signature:          [4]byte{'D', 'I', 'R', 'C'},
			Version:            2,
			NumberIndexEntries: 0,
		},
		Objects: make([]*IndexEntry, 0),
	}
}

type FixedIndexEntry struct {
	Ctime     uint32 // 16
	Ctimenano uint32 // 20

	Mtime int64 // 24

	Dev uint32 // 32
	Ino uint32 // 36

	Mode EntryMode // 40

	Uid uint32 // 44
	Gid uint32 // 48

	Fsize uint32 // 52

	Sha1 Sha1 // 72

	Flags uint16 // 74
}

func (i FixedIndexEntry) ExtendedFlag() bool {
	return ((i.Flags >> 14) & 0x1) == 1
}
func (i *FixedIndexEntry) SetExtendedFlag(val bool) {
	if val {
		i.Flags |= (0x1 << 14)
	} else {
		i.Flags &^= (0x1 << 14)
	}
}

// Refreshes the stat information for this entry using the file
// file
func (i *FixedIndexEntry) RefreshStat(f File) error {
	log.Printf("Refreshing stat info for %v\n", f)
	// FIXME: Add other stat info here too, but these are the
	// most important ones and the onlye ones that the os package
	// exposes in a cross-platform way.
	stat, err := f.Lstat()
	if err != nil {
		return err
	}
	fmtime, err := f.MTime()
	if err != nil {
		return err
	}
	i.Mtime = fmtime
	i.Fsize = uint32(stat.Size())
	i.Ctime, i.Ctimenano = f.CTime()
	i.Ino = f.INode()
	return nil
}

// Refreshes the stat information for this entry in the index against
// the stat info on the filesystem for things that we know about.
func (i *FixedIndexEntry) CompareStat(f File) error {
	log.Printf("Comparing stat info for %v\n", f)
	// FIXME: Add other stat info here too, but these are the
	// most important ones and the onlye ones that the os package
	// exposes in a cross-platform way.
	stat, err := f.Lstat()
	if err != nil {
		return err
	}
	fmtime, err := f.MTime()
	if err != nil {
		return err
	}
	if i.Mtime != fmtime {
		return fmt.Errorf("MTime does not match for %v", f)
	}
	if f.IsSymlink() {
		dst, err := os.Readlink(string(f))
		if err != nil {
			return err
		}
		if int(i.Fsize) != len(dst) {
			return fmt.Errorf("Size does not match for symlink %v", f)
		}
	} else {
		if i.Fsize != uint32(stat.Size()) {
			return fmt.Errorf("Size does not match for %v", f)
		}
	}
	ctime, ctimenano := f.CTime()
	if i.Ctime != ctime || i.Ctimenano != ctimenano {
		return fmt.Errorf("CTime does not match for %v", f)
	}
	if i.Ino != f.INode() {
		return fmt.Errorf("INode does not match for %v", f)
	}
	return nil
}

// Refreshes the stat information for an index entry by comparing
// it against the path in the index.
func (ie *IndexEntry) RefreshStat(c *Client) error {
	f, err := ie.PathName.FilePath(c)
	if err != nil {
		return err
	}
	return ie.FixedIndexEntry.RefreshStat(f)
}

// Reads the index file from the GitDir and returns a Index object.
// If the index file does not exist, it will return a new empty Index.
func (d GitDir) ReadIndex() (*Index, error) {
	var file *os.File
	var err error
	if ifile := os.Getenv("GIT_INDEX_FILE"); ifile != "" {
		log.Println("Using index file", ifile)
		file, err = os.Open(ifile)
	} else {

		file, err = d.Open("index")
	}

	if err != nil {
		if os.IsNotExist(err) {
			// Is the file doesn't exist, treat it
			// as a new empty index.
			return &Index{
				fixedGitIndex{
					[4]byte{'D', 'I', 'R', 'C'},
					2, // version 2
					0, // no entries
				},
				make([]*IndexEntry, 0),
			}, nil
		}
		return nil, err
	}
	defer file.Close()

	var i fixedGitIndex
	binary.Read(file, binary.BigEndian, &i)
	if i.Signature != [4]byte{'D', 'I', 'R', 'C'} {
		return nil, InvalidIndex
	}
	if i.Version < 2 || i.Version > 4 {
		return nil, InvalidIndex
	}
	log.Println("Index version", i.Version)

	var idx uint32
	indexes := make([]*IndexEntry, i.NumberIndexEntries, i.NumberIndexEntries)
	for idx = 0; idx < i.NumberIndexEntries; idx += 1 {
		index, err := ReadIndexEntry(file, i.Version)
		if err != nil {
			log.Println(err)
		} else {
			log.Println("Read entry for ", index.PathName)
			indexes[idx] = index
		}
	}
	return &Index{i, indexes}, nil
}

func ReadIndexEntry(file *os.File, indexVersion uint32) (*IndexEntry, error) {
	log.Printf("Reading index entry from %v assuming index version %d\n", file.Name(), indexVersion)
	if indexVersion < 2 || indexVersion > 3 {
		return nil, fmt.Errorf("Unsupported index version.")
	}
	var f FixedIndexEntry
	var name []byte
	if err := binary.Read(file, binary.BigEndian, &f); err != nil {
		return nil, err
	}

	var v3e *V3IndexExtensions
	if f.ExtendedFlag() {
		if indexVersion < 3 {
			return nil, InvalidIndex
		}
		v3e = &V3IndexExtensions{}
		if err := binary.Read(file, binary.BigEndian, v3e); err != nil {
			return nil, err
		}
	}

	var nameLength uint16
	nameLength = f.Flags & 0x0FFF

	if nameLength&0xFFF != 0xFFF {
		name = make([]byte, nameLength, nameLength)
		n, err := file.Read(name)
		if err != nil {
			panic("I don't know what to do")
		}
		if n != int(nameLength) {
			panic(fmt.Sprintf("Error reading the name read %d (got :%v)", n, string(name[:n])))
		}

		// I don't understand where this +4 comes from, but it seems to work
		// out with how the c git implementation calculates the padding..
		//
		// The definition of the index file format at:
		// https://github.com/git/git/blob/master/Documentation/technical/index-format.txt
		// claims that there should be "1-8 nul bytes as necessary to pad the entry to a multiple of eight
		// bytes while keeping the name NUL-terminated."
		//
		// The fixed size of the header is 82 bytes if you add up all the types.
		// the length of the name is nameLength bytes, so according to the spec
		// this *should* be 8 - ((82 + nameLength) % 8) bytes of padding.
		// But reading existant index files, there seems to be an extra 4 bytes
		// incorporated into the index size calculation.
		sz := uint16(82)

		if f.ExtendedFlag() {
			// Add 2 bytes if the extended flag is set for the V3 extensions
			sz += 2
		}
		expectedOffset := 8 - ((sz + nameLength + 4) % 8)
		file.Seek(int64(expectedOffset), 1)
	} else {

		nbyte := make([]byte, 1, 1)

		// This algorithm isn't very efficient, reading one byte at a time, but we
		// reserve a big space for name to make it slightly more efficient, since
		// we know it's a large name
		name = make([]byte, 0, 8192)
		for _, err := file.Read(nbyte); nbyte[0] != 0; _, err = file.Read(nbyte) {
			if err != nil {
				return nil, err
			}
			name = append(name, nbyte...)
		}
		off, err := file.Seek(0, io.SeekCurrent)
		if err != nil {
			return nil, err
		}
		// The mystery 4 appears again.
		padding := 8 - ((off + 4) % 8)
		if _, err := file.Seek(padding, io.SeekCurrent); err != nil {
			return nil, err
		}
	}
	return &IndexEntry{f, v3e, IndexPath(name)}, nil
}

// A Stage represents a git merge stage in the index.
type Stage uint8

// Valid merge stages.
const (
	Stage0 = Stage(iota)
	Stage1
	Stage2
	Stage3
)

func (g *Index) SetSkipWorktree(c *Client, path IndexPath, value bool) error {
	for _, entry := range g.Objects {
		if entry.PathName == path {
			if entry.Stage() != Stage0 {
				return fmt.Errorf("Can not set skip worktree on unmerged paths")
			}
			entry.SetSkipWorktree(value)
			break
		}
	}
	if g.Version <= 2 && value {
		g.Version = 3
	}
	return nil
}

// Adds an entry to the index with Sha1 s and stage stage during a merge.
// If an entry already exists for this pathname/stage, it will be overwritten,
// otherwise it will be added if createEntry is true, and return an error if not.
//
// As a special case, if something is added as Stage0, then Stage1-3 entries
// will be removed.
func (g *Index) AddStage(c *Client, path IndexPath, mode EntryMode, s Sha1, stage Stage, size uint32, mtime int64, opts UpdateIndexOptions) error {
	if stage == Stage0 {
		defer g.RemoveUnmergedStages(c, path)
	}

	replaceEntriesCheck := func() error {
		if stage != Stage0 {
			return nil
		}
		// If replace is true then we search for any entries that
		//  should be replaced with this one.
		newObjects := make([]*IndexEntry, 0, len(g.Objects))
		for _, e := range g.Objects {
			if strings.HasPrefix(string(e.PathName), string(path)+"/") {
				if !opts.Replace {
					return fmt.Errorf("There is an existing file %s under %s, should it be replaced?", e.PathName, path)
				}
				continue
			} else if strings.HasPrefix(string(path), string(e.PathName)+"/") {
				if !opts.Replace {
					return fmt.Errorf("There is a parent file %s above %s, should it be replaced?", e.PathName, path)
				}
				continue
			}

			newObjects = append(newObjects, e)
		}

		g.Objects = newObjects

		return nil
	}

	// Update the existing stage, if it exists.
	for _, entry := range g.Objects {
		if entry.PathName == path && entry.Stage() == stage {
			if err := replaceEntriesCheck(); err != nil {
				return err
			}

			file, _ := path.FilePath(c)
			if file.Exists() && stage == Stage0 {
				// FIXME: mtime/fsize/etc and ctime should either all be
				// from the filesystem, or all come from the caller
				// For now we just refresh the stat, and then overwrite with
				// the stuff from the caller.
				log.Println("Refreshing stat for", path)
				if err := entry.RefreshStat(c); err != nil {
					return err
				}
			}
			entry.Sha1 = s
			entry.Mtime = mtime
			entry.Fsize = size
			return nil
		}
	}

	if !opts.Add {
		return fmt.Errorf("%v not found in index", path)
	}
	// There was no path/stage combo already in the index. Add it.

	// According to the git documentation:
	// Flags is
	//    A 16-bit 'flags' field split into (high to low bits)
	//
	//       1-bit assume-valid flag
	//
	//       1-bit extended flag (must be zero in version 2)
	//
	//       2-bit stage (during merge)
	//       12-bit name length if the length is less than 0xFFF; otherwise 0xFFF
	//     is stored in this field.

	// So we'll construct the flags based on what we know.

	var flags = uint16(stage) << 12 // start with the stage.
	// Add the name length.
	if len(path) >= 0x0FFF {
		flags |= 0x0FFF
	} else {
		flags |= (uint16(len(path)) & 0x0FFF)
	}

	if err := replaceEntriesCheck(); err != nil {
		return err
	}
	newentry := &IndexEntry{
		FixedIndexEntry{
			0, //uint32(csec),
			0, //uint32(cnano),
			mtime,
			0, //uint32(stat.Dev),
			0, //uint32(stat.Ino),
			mode,
			0, //stat.Uid,
			0, //stat.Gid,
			size,
			s,
			flags,
		},
		&V3IndexExtensions{},
		path,
	}
	newentry.RefreshStat(c)

	g.Objects = append(g.Objects, newentry)
	g.NumberIndexEntries += 1
	sort.Sort(ByPath(g.Objects))
	return nil
}

// Remove any unmerged (non-stage 0) stage from the index for the given path
func (g *Index) RemoveUnmergedStages(c *Client, path IndexPath) error {
	// There are likely 3 things being deleted, so make a new slice
	newobjects := make([]*IndexEntry, 0, len(g.Objects))
	for _, entry := range g.Objects {
		stage := entry.Stage()
		if entry.PathName == path && stage == Stage0 {
			newobjects = append(newobjects, entry)
		} else if entry.PathName == path && stage != Stage0 {
			// do not add it, it's the wrong stage.
		} else {
			// It's a different Pathname, keep it.
			newobjects = append(newobjects, entry)
		}
	}
	g.Objects = newobjects
	g.NumberIndexEntries = uint32(len(newobjects))
	return nil
}

// Adds a file to the index, without writing it to disk.
// To write it to disk after calling this, use GitIndex.WriteIndex
//
// This will do the following:
// 1. write git object blob of file contents to .git/objects
// 2. normalize os.File name to path relative to gitRoot
// 3. search GitIndex for normalized name
//	if GitIndexEntry found
//		update GitIndexEntry to point to the new object blob
// 	else
// 		add new GitIndexEntry if not found and createEntry is true, error otherwise
//
func (g *Index) AddFile(c *Client, file File, opts UpdateIndexOptions) error {
	name, err := file.IndexPath(c)
	if err != nil {
		return err
	}

	mtime, err := file.MTime()
	if err != nil {
		return err
	}

	fsize := uint32(0)
	fstat, err := file.Lstat()
	if err == nil {
		fsize = uint32(fstat.Size())
	}
	if fstat.IsDir() {
		return fmt.Errorf("Must add a file, not a directory.")
	}
	var mode EntryMode

	var hash Sha1
	if file.IsSymlink() {
		mode = ModeSymlink
		contents, err := os.Readlink(string(file))
		if err != nil {
			return err
		}
		hash1, err := c.WriteObject("blob", []byte(contents))
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error storing object: %s", err)
		}
		hash = hash1
	} else {
		mode = ModeBlob
		contents, err := ioutil.ReadFile(string(file))
		if err != nil {
			return err
		}
		hash1, err := c.WriteObject("blob", contents)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error storing object: %s", err)
			return err
		}
		hash = hash1
	}

	return g.AddStage(
		c,
		name,
		mode,
		hash,
		Stage0,
		fsize,
		mtime,
		opts,
	)
}

type IndexStageEntry struct {
	IndexPath
	Stage
}

func (i *Index) GetStageMap() map[IndexStageEntry]*IndexEntry {
	r := make(map[IndexStageEntry]*IndexEntry)
	for _, entry := range i.Objects {
		r[IndexStageEntry{entry.PathName, entry.Stage()}] = entry
	}
	return r
}

type UnmergedPath struct {
	Stage1, Stage2, Stage3 *IndexEntry
}

func (i *Index) GetUnmerged() map[IndexPath]*UnmergedPath {
	r := make(map[IndexPath]*UnmergedPath)
	for _, entry := range i.Objects {
		if entry.Stage() != Stage0 {
			e, ok := r[entry.PathName]
			if !ok {
				e = &UnmergedPath{}
				r[entry.PathName] = e
			}
			switch entry.Stage() {
			case Stage1:
				e.Stage1 = entry
			case Stage2:
				e.Stage2 = entry
			case Stage3:
				e.Stage3 = entry
			}
		}
	}
	return r
}

// Remove the first instance of file from the index. (This will usually
// be stage 0.)
func (g *Index) RemoveFile(file IndexPath) {
	for i, entry := range g.Objects {
		if entry.PathName == file {
			g.Objects = append(g.Objects[:i], g.Objects[i+1:]...)
			g.NumberIndexEntries -= 1
			return
		}
	}
}

// This will write a new index file to w by doing the following:
// 1. Sort the objects in g.Index to ascending order based on name and update
//    g.NumberIndexEntries
// 2. Write g.fixedGitIndex to w
// 3. for each entry in g.Objects, write it to w.
// 4. Write the Sha1 of the contents of what was written
func (g Index) WriteIndex(file io.Writer) error {
	sort.Sort(ByPath(g.Objects))
	g.NumberIndexEntries = uint32(len(g.Objects))
	s := sha1.New()
	w := io.MultiWriter(file, s)
	binary.Write(w, binary.BigEndian, g.fixedGitIndex)
	for _, entry := range g.Objects {
		if err := binary.Write(w, binary.BigEndian, entry.FixedIndexEntry); err != nil {
			return err
		}
		if entry.ExtendedFlag() {
			if g.Version == 2 || entry.V3IndexExtensions == nil {
				return InvalidIndex
			}
			if err := binary.Write(w, binary.BigEndian, *entry.V3IndexExtensions); err != nil {
				return err
			}
		}
		if err := binary.Write(w, binary.BigEndian, []byte(entry.PathName)); err != nil {
			return err
		}
		sz := 82
		if entry.ExtendedFlag() {
			sz += 2
		}
		padding := 8 - ((sz + len(entry.PathName) + 4) % 8)
		p := make([]byte, padding)
		if err := binary.Write(w, binary.BigEndian, p); err != nil {
			return err
		}
	}
	binary.Write(w, binary.BigEndian, s.Sum(nil))
	return nil
}

// Looks up the Sha1 of path currently stored in the index.
// Will return the 0 Sha1 if not found.
func (g Index) GetSha1(path IndexPath) Sha1 {
	for _, entry := range g.Objects {
		if entry.PathName == path {
			return entry.Sha1
		}
	}
	return Sha1{}
}

// Implement the sort interface on *GitIndexEntry, so that
// it's easy to sort by name.
type ByPath []*IndexEntry

func (g ByPath) Len() int      { return len(g) }
func (g ByPath) Swap(i, j int) { g[i], g[j] = g[j], g[i] }
func (g ByPath) Less(i, j int) bool {
	if g[i].PathName == g[j].PathName {
		return g[i].Stage() < g[j].Stage()
	}
	ibytes := []byte(g[i].PathName)
	jbytes := []byte(g[j].PathName)
	for k := range ibytes {
		if k >= len(jbytes) {
			// We reached the end of j and there was stuff
			// leftover in i, so i > j
			return false
		}

		// If a character is not equal, return if it's
		// less or greater
		if ibytes[k] < jbytes[k] {
			return true
		} else if ibytes[k] > jbytes[k] {
			return false
		}
	}
	// Everything equal up to the end of i, and there is stuff
	// left in j, so i < j
	return true
}

// Replaces the index of Client with the the tree from the provided Treeish.
// if PreserveStatInfo is true, the stat information in the index won't be
// modified for existing entries.
func (g *Index) ResetIndex(c *Client, tree Treeish) error {
	newEntries, err := expandGitTreeIntoIndexes(c, tree, true, false, false)
	if err != nil {
		return err
	}
	g.NumberIndexEntries = uint32(len(newEntries))
	g.Objects = newEntries
	return nil
}

func (g Index) String() string {
	ret := ""

	for _, i := range g.Objects {
		ret += fmt.Sprintf("%v %v %v\n", i.Mode, i.Sha1, i.PathName)
	}
	return ret
}

type IndexMap map[IndexPath]*IndexEntry

func (i *Index) GetMap() IndexMap {
	r := make(IndexMap)
	for _, entry := range i.Objects {
		r[entry.PathName] = entry
	}
	return r
}

func (im IndexMap) Contains(path IndexPath) bool {
	if _, ok := im[path]; ok {
		return true
	}

	// Check of there is a directory named path in the IndexMap
	return im.HasDir(path)
}

func (im IndexMap) HasDir(path IndexPath) bool {
	for _, im := range im {
		if strings.HasPrefix(string(im.PathName), string(path+"/")) {
			return true
		}
	}
	return false
}
