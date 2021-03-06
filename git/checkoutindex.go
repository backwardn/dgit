package git

import (
	"bufio"
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// CheckoutIndexOptions represents the options that may be passed to
// "git checkout-index"
type CheckoutIndexOptions struct {
	UpdateStat bool

	Quiet bool
	Force bool
	All   bool

	NoCreate bool

	Prefix string

	// Stage not implemented
	Stage string // <number>|all

	Temp bool

	// Stdin implies checkout-index with the --stdin parameter.
	// nil implies it wasn't passed.
	Stdin         io.Reader // nil implies no --stdin param passed
	NullTerminate bool
}

// Performs CheckoutIndex on the Stdin io.Reader from opts, with the git index
// passed as a parameter.
func CheckoutIndexFromReaderUncommited(c *Client, idx *Index, opts CheckoutIndexOptions) error {
	if opts.Stdin == nil {
		return fmt.Errorf("Invalid Reader for opts.Stdin")
	}
	reader := bufio.NewReader(opts.Stdin)

	var delim byte = '\n'
	if opts.NullTerminate {
		delim = 0
	}

	var f File
	for s, err := reader.ReadString(delim); err == nil; s, err = reader.ReadString(delim) {
		f = File(strings.TrimSuffix(s, string(delim)))

		e := CheckoutIndexUncommited(c, idx, opts, []File{f})
		if e != nil {
			fmt.Fprintln(os.Stderr, e)
		}
	}
	return nil
}

// Performs a CheckoutIndex on the files read from opts.Stdin
func CheckoutIndexFromReader(c *Client, opts CheckoutIndexOptions) error {
	idx, err := c.GitDir.ReadIndex()
	if err != nil {
		return err
	}
	return CheckoutIndexFromReaderUncommited(c, idx, opts)
}

// Handles checking out a file when --temp is specified on the command line.
func checkoutTemp(c *Client, entry *IndexEntry, opts CheckoutIndexOptions) (string, error) {
	// I don't know where ".merged_file" comes from
	// for checkout-index, but it's what the real
	// git client seems to use for a prefix..
	tmpfile, err := ioutil.TempFile(".", ".merge_file_")
	if err != nil {
		return "", err
	}
	defer tmpfile.Close()

	obj, err := c.GetObject(entry.Sha1)
	if err != nil {
		return "", err
	}
	_, err = tmpfile.Write(obj.GetContent())
	if err != nil {
		return "", err
	}

	os.Chmod(tmpfile.Name(), os.FileMode(entry.Mode))
	return tmpfile.Name(), nil
}

// Checks out a given index entry.
func checkoutFile(c *Client, entry *IndexEntry, opts CheckoutIndexOptions) error {
	f, err := entry.PathName.FilePath(c)
	if err != nil {
		return err
	}
	f = File(opts.Prefix) + f
	if f.Exists() && !opts.Force {
		if !opts.Quiet {
			return fmt.Errorf("%v already exists, no checkout", entry.PathName.String())
		}
		return nil
	}

	obj, err := c.GetObject(entry.Sha1)
	if err != nil {
		return err
	}
	if !opts.NoCreate {
		fmode := os.FileMode(entry.Mode)
		if f.Exists() && f.IsDir() {
			if err := os.RemoveAll(f.String()); err != nil {
				return err
			}
		}

		p := filepath.Dir(f.String())
		if f := File(p); !f.Exists() {
			if err := os.MkdirAll(p, 0777); err != nil {
				return err
			}
		} else if !f.IsDir() {
			// FIXME: This shouldn't be required, this
			// should be handled by being returned by
			// ls-files -k before we get to this point.
			if err := os.Remove(f.String()); err != nil {
				return err
			}
			if err := os.MkdirAll(p, 0777); err != nil {
				return err
			}
		}
		err := ioutil.WriteFile(f.String(), obj.GetContent(), fmode)
		if err != nil {
			return err
		}
		os.Chmod(f.String(), os.FileMode(entry.Mode))
	}

	// Update the stat information, but only if it's the same
	// file name. We only change the mtime, and ctime because the only
	// other thing we track is the file size, and that couldn't
	// have changed.
	// Don't change the stat info if there's a prefix, because
	// if we checkout out into a prefix, it means we haven't
	// touched the index.
	if opts.Prefix == "" && opts.UpdateStat {
		if err := entry.RefreshStat(c); err != nil {
			return err
		}
	}
	return nil
}

// Same as "git checkout-index", except the Index is passed as a parameter (and
// may not have been written to disk yet). You likely want CheckoutIndex instead.
//
// (This is primarily for read-tree to be able to update the filesystem with the
// -u parameter.)
func CheckoutIndexUncommited(c *Client, idx *Index, opts CheckoutIndexOptions, files []File) error {
	if opts.All {
		files = make([]File, 0, len(idx.Objects))
		for _, entry := range idx.Objects {
			f, err := entry.PathName.FilePath(c)
			if err != nil {
				return err
			}
			files = append(files, f)
		}
	}

	killed, err := LsFiles(c, LsFilesOptions{Killed: true}, files)
	if err != nil {
		return err
	}
	if len(killed) > 0 {
		if !opts.Force {
			msg := ""
			for i, path := range killed {
				if i > 0 {
					msg += "\n"

				}
				f, err := path.PathName.FilePath(c)
				if err != nil {
					return err
				}
				msg += fmt.Sprintf("fatal: cannot create directory at '%v': File exists", f)
			}
			return fmt.Errorf("%v", msg)
		} else {
			for _, file := range killed {
				f, err := file.PathName.FilePath(c)
				if err != nil {
					return err
				}

				if err := os.RemoveAll(f.String()); err != nil {
					return err
				}
			}
		}
	}

	var stageMap map[IndexStageEntry]*IndexEntry
	if opts.Stage == "all" {
		// This is only used if stage==all, but we don't want to reallocate
		// it every iteration of the loop, so we just define it as a var
		// and let it stay nil unless stage=="all"
		stageMap = idx.GetStageMap()
	}
	var delim byte = '\n'
	if opts.NullTerminate {
		delim = 0
	}

	for _, file := range files {
		fname := File(file)
		indexpath, err := fname.IndexPath(c)
		if err != nil {
			if !opts.Quiet {
				fmt.Fprintf(os.Stderr, "%v\n", err)
			}
			continue
		}

		if opts.Stage == "all" {
			if _, ok := stageMap[IndexStageEntry{indexpath, Stage0}]; ok {
				if !opts.Quiet {
					// This error doesn't make any sense to me,
					// but it's what the official git client says when
					// you try and checkout-index --stage=all a file that isn't
					// in conflict.
					fmt.Fprintf(os.Stderr, "git checkout-index: %v does not exist at stage 0\n", indexpath)
				}
				continue
			}
			if stg1, s1ok := stageMap[IndexStageEntry{indexpath, Stage1}]; s1ok {
				name, err := checkoutTemp(c, stg1, opts)
				if err != nil {
					fmt.Fprintln(os.Stderr, err)
				}
				fmt.Print(name, " ")
			} else {
				fmt.Print(". ")
			}
			if stg2, s2ok := stageMap[IndexStageEntry{indexpath, Stage2}]; s2ok {
				name, err := checkoutTemp(c, stg2, opts)
				if err != nil {
					fmt.Fprintln(os.Stderr, err)
				}
				fmt.Print(name, " ")
			} else {
				fmt.Print(". ")
			}
			if stg3, s3ok := stageMap[IndexStageEntry{indexpath, Stage3}]; s3ok {
				name, err := checkoutTemp(c, stg3, opts)
				if err != nil {
					fmt.Fprintln(os.Stderr, err)
				}
				fmt.Printf("%s\t%s%c", name, stg3.PathName, delim)
			} else {
				fmt.Printf(".\t%s%c", stg3.PathName, delim)
			}
			continue
		}

		for _, entry := range idx.Objects {
			if entry.PathName != indexpath {
				continue
			}

			if !opts.Temp && !opts.Force && opts.Prefix == "" && entry.PathName.IsClean(c, entry.Sha1) {
				// don't bother checkout out the file
				// if it's already clean. This makes us less
				// likely to avoid GetObject have an error
				// trying to read from a packfile (which isn't
				// supported yet.)
				// We don't check this if there's a prefix, since it's not checking out
				// into the same location as the index.
				// FIXME: This should use stat information, not hash
				// the whole file.
				continue
			}

			switch opts.Stage {
			case "":
				if entry.Stage() == Stage0 {
					var name string
					if opts.Temp {
						name, err = checkoutTemp(c, entry, opts)
						if name != "" {
							fmt.Printf("%v\t%v%c", name, entry.PathName, delim)
						}
					} else {
						err = checkoutFile(c, entry, opts)
					}
				} else {
					return fmt.Errorf("Index has unmerged entries. Aborting.")
				}
			case "1", "2", "3":
				stg, _ := strconv.Atoi(opts.Stage)
				if entry.Stage() == Stage(stg) {
					var name string

					if opts.Temp {
						name, err = checkoutTemp(c, entry, opts)
						if name != "" {
							fmt.Printf("%v\t%v%c", name, entry.PathName, delim)
						}

					} else {
						err = checkoutFile(c, entry, opts)
					}
				}
			default:
				return fmt.Errorf("Invalid stage: %v", opts.Stage)
			}
			if err != nil {
				fmt.Fprintln(os.Stderr, err)
			}

		}
	}

	if opts.UpdateStat {
		f, err := c.GitDir.Create(File("index"))
		if err != nil {
			return err
		}
		defer f.Close()
		return idx.WriteIndex(f)
	}
	return nil
}

// CheckoutIndex implements the "git checkout-index" subcommand of git.
func CheckoutIndex(c *Client, opts CheckoutIndexOptions, files []File) error {
	if len(files) != 0 && opts.All {
		return fmt.Errorf("Can not mix --all and named files")
	}

	idx, err := c.GitDir.ReadIndex()
	if err != nil {
		return err
	}
	if opts.Stdin == nil {
		return CheckoutIndexUncommited(c, idx, opts, files)
	} else {
		if len(files) != 0 {
			return fmt.Errorf("Can not mix --stdin and paths on command line")
		}
		return CheckoutIndexFromReaderUncommited(c, idx, opts)
	}
}
