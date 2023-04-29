// License: GPLv3 Copyright: 2023, Kovid Goyal, <kovid at kovidgoyal.net>

package transfer

import (
	"fmt"
	"io/fs"
	"kitty/tools/utils"
	"kitty/tools/wcswidth"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"golang.org/x/exp/slices"
)

var _ = fmt.Print

type FileType int

const (
	REGULAR_FILE FileType = iota
	SYMLINK_FILE
	DIRECTORY_FILE
	LINK_FILE
)

func (self FileType) ShortText() string {
	switch self {
	case REGULAR_FILE:
		return "fil"
	case DIRECTORY_FILE:
		return "dir"
	case SYMLINK_FILE:
		return "sym"
	case LINK_FILE:
		return "lnk"
	}
	return "und"
}

func (self FileType) Color() string {
	switch self {
	case REGULAR_FILE:
		return "yellow"
	case DIRECTORY_FILE:
		return "magenta"
	case SYMLINK_FILE:
		return "blue"
	case LINK_FILE:
		return "green"
	}
	return ""
}

func (self FileType) String() string {
	switch self {
	case REGULAR_FILE:
		return "FileType.Regular"
	case DIRECTORY_FILE:
		return "FileType.Directory"
	case SYMLINK_FILE:
		return "FileType.SymbolicLink"
	case LINK_FILE:
		return "FileType.Link"
	}
	return "FileType.Unknown"
}

type FileState int

const (
	WAITING_FOR_START FileState = iota
	WAITING_FOR_DATA
	TRANSMITTING
	FINISHED
	ACKNOWLEDGED
)

type FileHash struct{ dev, inode uint64 }

type File struct {
	file_hash                                             FileHash
	file_type                                             FileType
	file_id, hard_link_target                             string
	local_path, symbolic_link_target, expanded_local_path string
	stat_result                                           fs.FileInfo
	state                                                 FileState
	display_name                                          string
	mtime                                                 time.Time
	file_size, bytes_to_transmit                          int64
	permissions                                           fs.FileMode
	remote_path                                           string
	rsync_capable, compression_capable                    bool
	remote_final_path                                     string
	remote_initial_size                                   int64
	err_msg                                               string
	actual_file                                           *os.File
	transmitted_bytes, reported_progress                  int64
	transmit_started_at, transmit_ended_at, done_at       time.Time
}

func get_remote_path(local_path string, remote_base string) string {
	if remote_base == "" {
		return filepath.ToSlash(local_path)
	}
	if strings.HasSuffix(remote_base, "/") {
		return filepath.Join(remote_base, filepath.Base(local_path))
	}
	return remote_base
}

func NewFile(local_path, expanded_local_path string, file_id int, stat_result fs.FileInfo, remote_base string, file_type FileType) *File {
	stat, ok := stat_result.Sys().(*syscall.Stat_t)
	if !ok {
		panic("This platform does not support getting file identities from stat results")
	}
	ans := File{
		local_path: local_path, expanded_local_path: expanded_local_path, file_id: fmt.Sprintf("%x", file_id),
		stat_result: stat_result, file_type: file_type, display_name: wcswidth.StripEscapeCodes(local_path),
		file_hash: FileHash{stat.Dev, stat.Ino}, mtime: stat_result.ModTime(),
		file_size: stat_result.Size(), bytes_to_transmit: stat_result.Size(),
		permissions: stat_result.Mode().Perm(), remote_path: filepath.ToSlash(get_remote_path(local_path, remote_base)),
		rsync_capable:       file_type == REGULAR_FILE && stat_result.Size() > 4096,
		compression_capable: file_type == REGULAR_FILE && stat_result.Size() > 4096 && should_be_compressed(expanded_local_path),
		remote_initial_size: -1,
	}
	return &ans
}

func process(opts *Options, paths []string, remote_base string, counter *int) (ans []*File, err error) {
	for _, x := range paths {
		expanded := expand_home(x)
		s, err := os.Lstat(expanded)
		if err != nil {
			return ans, fmt.Errorf("Failed to stat %s with error: %w", x, err)
		}
		if s.IsDir() {
			*counter += 1
			ans = append(ans, NewFile(x, expanded, *counter, s, remote_base, DIRECTORY_FILE))
			new_remote_base := remote_base
			if new_remote_base != "" {
				new_remote_base = strings.TrimRight(new_remote_base, "/") + "/" + filepath.Base(x) + "/"
			} else {
				new_remote_base = strings.TrimRight(filepath.ToSlash(x), "/") + "/"
			}
			contents, err := os.ReadDir(expanded)
			if err != nil {
				return ans, fmt.Errorf("Failed to read the directory %s with error: %w", x, err)
			}
			new_paths := make([]string, len(contents))
			for i, y := range contents {
				new_paths[i] = filepath.Join(x, y.Name())
			}
			new_ans, err := process(opts, new_paths, new_remote_base, counter)
			if err != nil {
				return ans, err
			}
			ans = append(ans, new_ans...)
		} else if s.Mode()&fs.ModeSymlink == fs.ModeSymlink {
			*counter += 1
			ans = append(ans, NewFile(x, expanded, *counter, s, remote_base, SYMLINK_FILE))
		} else if s.Mode().IsRegular() {
			*counter += 1
			ans = append(ans, NewFile(x, expanded, *counter, s, remote_base, REGULAR_FILE))
		}
	}
	return
}

func process_mirrored_files(opts *Options, args []string) (ans []*File, err error) {
	paths := utils.Map(func(x string) string { return abspath(x) }, args)
	common_path := utils.Commonpath(paths...)
	home := strings.TrimRight(home_path(), string(filepath.Separator))
	if common_path != "" && strings.HasPrefix(common_path, home+string(filepath.Separator)) {
		paths = utils.Map(func(x string) string {
			r, _ := filepath.Rel(home, x)
			return filepath.Join("~", r)
		}, paths)
	}
	counter := 0
	return process(opts, paths, "", &counter)
}

func process_normal_files(opts *Options, args []string) (ans []*File, err error) {
	if len(args) < 2 {
		return ans, fmt.Errorf("Must specify at least one local path and one remote path")
	}
	args = slices.Clone(args)
	remote_base := filepath.ToSlash(args[len(args)-1])
	args = args[:len(args)-1]
	if len(args) > 1 && !strings.HasSuffix(remote_base, "/") {
		remote_base += "/"
	}
	paths := utils.Map(func(x string) string { return abspath(x) }, args)
	counter := 0
	return process(opts, paths, remote_base, &counter)
}

func files_for_send(opts *Options, args []string) (files []*File, err error) {
	if opts.Mode == "mirror" {
		files, err = process_mirrored_files(opts, args)
	} else {
		files, err = process_normal_files(opts, args)
	}
	if err != nil {
		return files, err
	}
	groups := make(map[FileHash][]*File, len(files))

	// detect hard links
	for _, f := range files {
		groups[f.file_hash] = append(groups[f.file_hash], f)
	}
	for _, group := range groups {
		if len(group) > 1 {
			for _, lf := range group[1:] {
				lf.file_type = LINK_FILE
				lf.hard_link_target = group[0].file_id
			}
		}
	}

	remove := make([]int, 0, len(files))
	// detect symlinks to other transferred files
	for i, f := range files {
		if f.file_type == SYMLINK_FILE {
			link_dest, err := os.Readlink(f.local_path)
			if err != nil {
				remove = append(remove, i)
				continue
			}
			f.symbolic_link_target = "path:" + link_dest
			is_abs := filepath.IsAbs(link_dest)
			q := link_dest
			if !is_abs {
				q = filepath.Join(filepath.Dir(f.local_path), link_dest)
			}
			st, err := os.Stat(q)
			if err == nil {
				stat, ok := st.Sys().(*syscall.Stat_t)
				if ok {
					fh := FileHash{stat.Dev, stat.Ino}
					gr, found := groups[fh]
					if found {
						g := utils.Filter(gr, func(x *File) bool {
							return os.SameFile(x.stat_result, st)
						})
						if len(g) > 0 {
							f.symbolic_link_target = "fid"
							if is_abs {
								f.symbolic_link_target = "fid_abs"
							}
							f.symbolic_link_target += ":" + g[0].file_id
						}
					}
				}
			}
		}
	}
	if len(remove) > 0 {
		for _, idx := range utils.Reverse(remove) {
			files[idx] = nil
			files = slices.Delete(files, idx, idx+1)
		}
	}
	return files, nil
}

func send_main(opts *Options, args []string) (err error) {
	fmt.Println("Scanning files…")
	files, err := files_for_send(opts, args)
	if err != nil {
		return err
	}
	fmt.Printf("Found %d files and directories, requesting transfer permission…", len(files))
	fmt.Println()

	return
}
