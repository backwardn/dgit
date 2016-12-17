package main

import (
	"fmt"
	"io/ioutil"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// A file represents a file (or directory) relative to os.Getwd()
type File string

func (f File) Exists() bool {
	if _, err := os.Stat(string(f)); os.IsNotExist(err) {
		return false
	}
	return true
}

func (f File) String() string {
	return string(f)
}

// GitDir is the .git directory of the current process.
type GitDir File

func (g GitDir) String() string {
	return string(g)
}

func (g GitDir) Exists() bool {
	return File(g).Exists()
}

// Returns a file named f, relative to GitDir
func (g GitDir) File(f File) File {
	return File(g) + "/" + f
}

func (g GitDir) WriteFile(f File, data []byte, perm os.FileMode) error {
	return ioutil.WriteFile(g.File(f).String(), data, perm)
}

// WorkDir is the top level of the work directory of the current process, or
// the empty string if the --bare option is provided
type WorkDir File

type Client struct {
	GitDir  GitDir
	WorkDir WorkDir
}

// Walks from the current directory to find a .git directory
func findGitDir() GitDir {
	startPath, err := os.Getwd()
	if err != nil {
		return ""
	}
	if dirinfo, err := os.Stat(startPath + "/.git"); err == nil && dirinfo.IsDir() {
		return GitDir(startPath) + "/.git"
	}
	pieces := strings.Split(startPath, "/")

	for i := len(pieces); i > 0; i -= 1 {
		dir := strings.Join(pieces[0:i], "/")
		if dirinfo, err := os.Stat(dir + "/.git"); err == nil && dirinfo.IsDir() {
			return GitDir(dir) + "/.git"
		}
	}
	return ""
}

func NewClient(gitDir, workDir string) (*Client, error) {
	gitdir := GitDir(gitDir)
	if gitdir == "" {
		gitdir = GitDir(os.Getenv("GIT_DIR"))
		if gitdir == "" {
			gitdir = findGitDir()
		}
	}

	if gitdir == "" || !gitdir.Exists() {
		return nil, fmt.Errorf("fatal: Not a git repository (or any parent)")
	}

	workdir := WorkDir(workDir)
	if workdir == "" {
		workdir = WorkDir(os.Getenv("GIT_WORK_TREE"))
		if workdir == "" {
			workdir = WorkDir(strings.TrimSuffix(gitdir.String(), "/.git"))
		}
		// TODO: Check the GIT_WORK_TREE os environment, then strip .git
		// from the gitdir if it doesn't exist.
	}
	return &Client{GitDir(gitdir), WorkDir(workdir)}, nil
}

/*
func (c *Client) GetHeadSha1() (Sha1, error) {
	panic("Not yet reimplemented")
		if headBranch := getHeadBranch(repo); headBranch != "" {
			return repo.GetCommitIdOfBranch(getHeadBranch(repo))
		}
		return "", InvalidHead
}
*/

func (c *Client) GetHeadBranch() string {
	file, _ := c.GitDir.Open("HEAD")
	value, _ := ioutil.ReadAll(file)
	if prefix := string(value[0:5]); prefix != "ref: " {
		panic("Could not understand HEAD pointer.")
	} else {
		ref := strings.Split(string(value[5:]), "/")
		if len(ref) != 3 {
			panic("Could not parse branch out of HEAD")
		}
		if ref[0] != "refs" || ref[1] != "heads" {
			panic("Unknown HEAD reference")
		}
		return strings.TrimSpace(ref[2])
	}
	return ""

}

func (c *Client) ExecEditor(f File) error {
	editor := os.Getenv("EDITOR")
	if editor == "" {
		fmt.Fprintf(os.Stderr, "Warning: EDITOR environment not set. Falling back on ed...\n")
		editor = "ed"
	}
	cmd := exec.Command(editor, f.String())
	return cmd.Run()
}

// Opens a file relative to GitDir. There should not be
// a leading slash.
func (gd GitDir) Open(f File) (*os.File, error) {
	return os.Open(gd.String() + "/" + f.String())
}

// Creates a file relative to GitDir. There should not be
// a leading slash.
func (gd GitDir) Create(f File) (*os.File, error) {
	return os.Create(gd.String() + "/" + f.String())
}
func (c *Client) ResetWorkTree() error {
	idx, err := c.GitDir.ReadIndex()
	if err != nil {
		return err
	}
	for _, indexEntry := range idx.Objects {
		obj, err := c.GetObject(indexEntry.Sha1)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Could not retrieve %x for %s: %s\n", indexEntry.Sha1, indexEntry.PathName, err)
			continue
		}
		if strings.Index(indexEntry.PathName, "/") > 0 {
			os.MkdirAll(filepath.Dir(indexEntry.PathName), 0755)
		}
		err = ioutil.WriteFile(indexEntry.PathName, obj.GetContent(), os.FileMode(indexEntry.Mode))
		if err != nil {
			continue
		}
		os.Chmod(indexEntry.PathName, os.FileMode(indexEntry.Mode))
	}
	return nil
}

func (c *Client) GetSymbolicRefCommit(r RefSpec) (CommitID, error) {
	file, err := c.GitDir.Open(File(r))
	if err != nil {
		return CommitID{}, err
	}
	data, err := ioutil.ReadAll(file)
	if err != nil {
		return CommitID{}, err
	}
	sha, err := Sha1FromString(string(data))
	return CommitID(sha), err
}
func (c *Client) GetBranchCommit(b string) (CommitID, error) {
	file, err := c.GitDir.Open(File("refs/heads/" + b))
	if err != nil {
		return CommitID{}, err
	}
	data, err := ioutil.ReadAll(file)
	if err != nil {
		return CommitID{}, err
	}
	sha, err := Sha1FromString(string(data))
	return CommitID(sha), err
}
