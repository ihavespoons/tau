package compaction

import (
	"sort"
	"strings"

	"github.com/ihavespoons/tau/ai"
)

// FileOps is the set of files a stretch of conversation touched.
//
// It is tracked separately from the summary because file paths are the one
// thing a summary must never paraphrase: the model that resumes will act on
// them. Letting the summarizer decide which files mattered would eventually
// lose one.
type FileOps struct {
	Read    map[string]bool
	Written map[string]bool
	Edited  map[string]bool
}

// NewFileOps returns an empty set.
func NewFileOps() *FileOps {
	return &FileOps{Read: map[string]bool{}, Written: map[string]bool{}, Edited: map[string]bool{}}
}

// AddFromMessage records the file operations in an assistant message's tool
// calls. Only the built-in file tools are recognized, matching Pi.
func (f *FileOps) AddFromMessage(m ai.Message) {
	var blocks ai.ContentList
	switch msg := m.(type) {
	case ai.AssistantMessage:
		blocks = msg.Content
	case *ai.AssistantMessage:
		blocks = msg.Content
	default:
		return
	}
	for _, b := range blocks {
		var call *ai.ToolCall
		switch tc := b.(type) {
		case ai.ToolCall:
			call = &tc
		case *ai.ToolCall:
			call = tc
		default:
			continue
		}
		path, ok := call.Arguments["path"].(string)
		if !ok || path == "" {
			continue
		}
		switch call.Name {
		case "read":
			f.Read[path] = true
		case "write":
			f.Written[path] = true
		case "edit":
			f.Edited[path] = true
		}
	}
}

// FileLists is the read/modified split carried on a compaction entry.
type FileLists struct {
	ReadFiles     []string `json:"readFiles"`
	ModifiedFiles []string `json:"modifiedFiles"`
}

// Lists collapses the operations into the two lists that go in the summary.
// A file that was both read and changed counts as changed only — the fact that
// it was read first is not what the next model needs to know.
func (f *FileOps) Lists() FileLists {
	modified := map[string]bool{}
	for p := range f.Edited {
		modified[p] = true
	}
	for p := range f.Written {
		modified[p] = true
	}

	out := FileLists{ReadFiles: []string{}, ModifiedFiles: []string{}}
	for p := range f.Read {
		if !modified[p] {
			out.ReadFiles = append(out.ReadFiles, p)
		}
	}
	for p := range modified {
		out.ModifiedFiles = append(out.ModifiedFiles, p)
	}
	sort.Strings(out.ReadFiles)
	sort.Strings(out.ModifiedFiles)
	return out
}

// AddLists folds a previous checkpoint's file lists back in, so tracking is
// cumulative across successive compactions rather than restarting each time.
func (f *FileOps) AddLists(l FileLists) {
	for _, p := range l.ReadFiles {
		f.Read[p] = true
	}
	for _, p := range l.ModifiedFiles {
		f.Edited[p] = true
	}
}

// FormatFileOperations renders the lists as the tagged block appended to a
// summary.
func FormatFileOperations(l FileLists) string {
	var sections []string
	if len(l.ReadFiles) > 0 {
		sections = append(sections, "<read-files>\n"+strings.Join(l.ReadFiles, "\n")+"\n</read-files>")
	}
	if len(l.ModifiedFiles) > 0 {
		sections = append(sections, "<modified-files>\n"+strings.Join(l.ModifiedFiles, "\n")+"\n</modified-files>")
	}
	if len(sections) == 0 {
		return ""
	}
	return "\n\n" + strings.Join(sections, "\n\n")
}
