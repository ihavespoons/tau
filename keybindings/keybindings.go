// Package keybindings is tau's key-to-action map: the table of actions a key
// can be bound to, the user's overrides from ~/.tau/agent/keybindings.json, and
// the matcher the interface asks "was this key bound to that action?".
//
// Port of Pi's core/keybindings.ts and pi-tui's keybindings.ts. The ids and
// default keys are Pi's, so a keybindings.json written for Pi works in tau
// unchanged — including the flat pre-namespace names, which are migrated on
// load.
//
// Not every id is wired to behaviour yet: the table is the whole of Pi's so
// that a config naming a binding tau has not reached is preserved rather than
// reported as a typo.
package keybindings

import "runtime"

// Binding identifies an action a key can be bound to.
type Binding string

// Editor bindings move the cursor and change text (pi-tui keybindings.ts:53-110).
const (
	EditorCursorUp            Binding = "tui.editor.cursorUp"
	EditorCursorDown          Binding = "tui.editor.cursorDown"
	EditorCursorLeft          Binding = "tui.editor.cursorLeft"
	EditorCursorRight         Binding = "tui.editor.cursorRight"
	EditorCursorWordLeft      Binding = "tui.editor.cursorWordLeft"
	EditorCursorWordRight     Binding = "tui.editor.cursorWordRight"
	EditorCursorLineStart     Binding = "tui.editor.cursorLineStart"
	EditorCursorLineEnd       Binding = "tui.editor.cursorLineEnd"
	EditorJumpForward         Binding = "tui.editor.jumpForward"
	EditorJumpBackward        Binding = "tui.editor.jumpBackward"
	EditorPageUp              Binding = "tui.editor.pageUp"
	EditorPageDown            Binding = "tui.editor.pageDown"
	EditorDeleteCharBackward  Binding = "tui.editor.deleteCharBackward"
	EditorDeleteCharForward   Binding = "tui.editor.deleteCharForward"
	EditorDeleteWordBackward  Binding = "tui.editor.deleteWordBackward"
	EditorDeleteWordForward   Binding = "tui.editor.deleteWordForward"
	EditorDeleteToLineStart   Binding = "tui.editor.deleteToLineStart"
	EditorDeleteToLineEnd     Binding = "tui.editor.deleteToLineEnd"
	EditorYank                Binding = "tui.editor.yank"
	EditorYankPop             Binding = "tui.editor.yankPop"
	EditorUndo                Binding = "tui.editor.undo"
)

// Input bindings act on the prompt as a whole (pi-tui keybindings.ts:111-114).
const (
	InputNewLine Binding = "tui.input.newLine"
	InputSubmit  Binding = "tui.input.submit"
	InputTab     Binding = "tui.input.tab"
	InputCopy    Binding = "tui.input.copy"
)

// Select bindings drive list pickers (pi-tui keybindings.ts:115-126).
const (
	SelectUp       Binding = "tui.select.up"
	SelectDown     Binding = "tui.select.down"
	SelectPageUp   Binding = "tui.select.pageUp"
	SelectPageDown Binding = "tui.select.pageDown"
	SelectConfirm  Binding = "tui.select.confirm"
	SelectCancel   Binding = "tui.select.cancel"
)

// App bindings are the agent's own (core/keybindings.ts:64-207).
const (
	AppInterrupt                Binding = "app.interrupt"
	AppClear                    Binding = "app.clear"
	AppExit                     Binding = "app.exit"
	AppSuspend                  Binding = "app.suspend"
	AppThinkingCycle            Binding = "app.thinking.cycle"
	AppModelCycleForward        Binding = "app.model.cycleForward"
	AppModelCycleBackward       Binding = "app.model.cycleBackward"
	AppModelSelect              Binding = "app.model.select"
	AppToolsExpand              Binding = "app.tools.expand"
	AppThinkingToggle           Binding = "app.thinking.toggle"
	AppSessionToggleNamedFilter Binding = "app.session.toggleNamedFilter"
	AppEditorExternal           Binding = "app.editor.external"
	AppMessageCopy              Binding = "app.message.copy"
	AppMessageFollowUp          Binding = "app.message.followUp"
	AppMessageDequeue           Binding = "app.message.dequeue"
	AppClipboardPasteImage      Binding = "app.clipboard.pasteImage"
	AppSessionNew               Binding = "app.session.new"
	AppSessionTree              Binding = "app.session.tree"
	AppSessionFork              Binding = "app.session.fork"
	AppSessionResume            Binding = "app.session.resume"
	AppTreeFoldOrUp             Binding = "app.tree.foldOrUp"
	AppTreeUnfoldOrDown         Binding = "app.tree.unfoldOrDown"
	AppTreeEditLabel            Binding = "app.tree.editLabel"
	AppTreeToggleLabelTimestamp Binding = "app.tree.toggleLabelTimestamp"
	AppSessionTogglePath        Binding = "app.session.togglePath"
	AppSessionToggleSort        Binding = "app.session.toggleSort"
	AppSessionRename            Binding = "app.session.rename"
	AppSessionDelete            Binding = "app.session.delete"
	AppSessionDeleteNoninvasive Binding = "app.session.deleteNoninvasive"
	AppModelsSave               Binding = "app.models.save"
	AppModelsEnableAll          Binding = "app.models.enableAll"
	AppModelsClearAll           Binding = "app.models.clearAll"
	AppModelsToggleProvider     Binding = "app.models.toggleProvider"
	AppModelsReorderUp          Binding = "app.models.reorderUp"
	AppModelsReorderDown        Binding = "app.models.reorderDown"
	AppTreeFilterDefault        Binding = "app.tree.filter.default"
	AppTreeFilterNoTools        Binding = "app.tree.filter.noTools"
	AppTreeFilterUserOnly       Binding = "app.tree.filter.userOnly"
	AppTreeFilterLabeledOnly    Binding = "app.tree.filter.labeledOnly"
	AppTreeFilterAll            Binding = "app.tree.filter.all"
	AppTreeFilterCycleForward   Binding = "app.tree.filter.cycleForward"
	AppTreeFilterCycleBackward  Binding = "app.tree.filter.cycleBackward"
)

// Definition is one row of the binding table.
type Definition struct {
	// ID is the binding's canonical id, e.g. "app.model.select".
	ID Binding
	// DefaultKeys are the keys bound when the user has not said otherwise. An
	// empty list means the action exists but has no key until one is assigned.
	DefaultKeys []string
	// Description is the one-line label shown wherever bindings are listed.
	Description string
}

// Definitions is the binding table in declaration order — the order bindings
// are listed in and written back to keybindings.json.
var Definitions = buildDefinitions()

var byID = func() map[Binding]Definition {
	m := make(map[Binding]Definition, len(Definitions))
	for _, d := range Definitions {
		m[d.ID] = d
	}
	return m
}()

// Lookup returns the definition of a binding.
func Lookup(id Binding) (Definition, bool) {
	d, ok := byID[id]
	return d, ok
}

// IsDefined reports whether id names a binding tau knows.
func IsDefined(id string) bool {
	_, ok := byID[Binding(id)]
	return ok
}

func buildDefinitions() []Definition {
	// Platform conditionals, verbatim from core/keybindings.ts.
	//
	// Windows has no job control, so ctrl+z would only eat a key; it also
	// spells paste ctrl+v at the terminal level, so the image paste moves to
	// alt+v to stay out of the way. On macOS alt+arrow is the native word/fold
	// motion and ctrl+arrow belongs to Mission Control, so alt is listed first
	// — the order decides which keys a help screen shows first, not which ones
	// work.
	suspend := []string{"ctrl+z"}
	pasteImage := []string{"ctrl+v"}
	if runtime.GOOS == "windows" {
		suspend = nil
		pasteImage = []string{"alt+v"}
	}
	fold := []string{"ctrl+left", "alt+left"}
	unfold := []string{"ctrl+right", "alt+right"}
	if runtime.GOOS == "darwin" {
		fold = []string{"alt+left", "ctrl+left"}
		unfold = []string{"alt+right", "ctrl+right"}
	}

	return []Definition{
		{EditorCursorUp, []string{"up"}, "Move cursor up"},
		{EditorCursorDown, []string{"down"}, "Move cursor down"},
		{EditorCursorLeft, []string{"left", "ctrl+b"}, "Move cursor left"},
		{EditorCursorRight, []string{"right", "ctrl+f"}, "Move cursor right"},
		{EditorCursorWordLeft, []string{"alt+left", "ctrl+left", "alt+b"}, "Move cursor word left"},
		{EditorCursorWordRight, []string{"alt+right", "ctrl+right", "alt+f"}, "Move cursor word right"},
		{EditorCursorLineStart, []string{"home", "ctrl+a"}, "Move to line start"},
		{EditorCursorLineEnd, []string{"end", "ctrl+e"}, "Move to line end"},
		{EditorJumpForward, []string{"ctrl+]"}, "Jump forward to character"},
		{EditorJumpBackward, []string{"ctrl+alt+]"}, "Jump backward to character"},
		{EditorPageUp, []string{"pageUp"}, "Page up"},
		{EditorPageDown, []string{"pageDown"}, "Page down"},
		{EditorDeleteCharBackward, []string{"backspace"}, "Delete character backward"},
		{EditorDeleteCharForward, []string{"delete", "ctrl+d"}, "Delete character forward"},
		{EditorDeleteWordBackward, []string{"ctrl+w", "alt+backspace"}, "Delete word backward"},
		{EditorDeleteWordForward, []string{"alt+d", "alt+delete"}, "Delete word forward"},
		{EditorDeleteToLineStart, []string{"ctrl+u"}, "Delete to line start"},
		{EditorDeleteToLineEnd, []string{"ctrl+k"}, "Delete to line end"},
		{EditorYank, []string{"ctrl+y"}, "Yank"},
		{EditorYankPop, []string{"alt+y"}, "Yank pop"},
		{EditorUndo, []string{"ctrl+-"}, "Undo"},

		{InputNewLine, []string{"shift+enter", "ctrl+j"}, "Insert newline"},
		{InputSubmit, []string{"enter"}, "Submit input"},
		{InputTab, []string{"tab"}, "Tab / autocomplete"},
		{InputCopy, []string{"ctrl+c"}, "Copy selection"},

		{SelectUp, []string{"up"}, "Move selection up"},
		{SelectDown, []string{"down"}, "Move selection down"},
		{SelectPageUp, []string{"pageUp"}, "Selection page up"},
		{SelectPageDown, []string{"pageDown"}, "Selection page down"},
		{SelectConfirm, []string{"enter"}, "Confirm selection"},
		{SelectCancel, []string{"escape", "ctrl+c"}, "Cancel selection"},

		{AppInterrupt, []string{"escape"}, "Cancel or abort"},
		{AppClear, []string{"ctrl+c"}, "Clear editor"},
		{AppExit, []string{"ctrl+d"}, "Exit when editor is empty"},
		{AppSuspend, suspend, "Suspend to background"},
		{AppThinkingCycle, []string{"shift+tab"}, "Cycle thinking level"},
		{AppModelCycleForward, []string{"ctrl+p"}, "Cycle to next model"},
		{AppModelCycleBackward, []string{"shift+ctrl+p"}, "Cycle to previous model"},
		{AppModelSelect, []string{"ctrl+l"}, "Open model selector"},
		{AppToolsExpand, []string{"ctrl+o"}, "Toggle tool output"},
		{AppThinkingToggle, []string{"ctrl+t"}, "Toggle thinking blocks"},
		{AppSessionToggleNamedFilter, []string{"ctrl+n"}, "Toggle named session filter"},
		{AppEditorExternal, []string{"ctrl+g"}, "Open external editor"},
		{AppMessageCopy, []string{"ctrl+x"}, "Copy message to clipboard"},
		{AppMessageFollowUp, []string{"alt+enter"}, "Queue follow-up message"},
		{AppMessageDequeue, []string{"alt+up"}, "Restore queued messages"},
		{AppClipboardPasteImage, pasteImage, "Paste image from clipboard (text fallback)"},
		{AppSessionNew, nil, "Start a new session"},
		{AppSessionTree, nil, "Open session tree"},
		{AppSessionFork, nil, "Fork current session"},
		{AppSessionResume, nil, "Resume a session"},
		{AppTreeFoldOrUp, fold, "Fold tree branch or move up"},
		{AppTreeUnfoldOrDown, unfold, "Unfold tree branch or move down"},
		{AppTreeEditLabel, []string{"shift+l"}, "Edit tree label"},
		{AppTreeToggleLabelTimestamp, []string{"shift+t"}, "Toggle tree label timestamps"},
		{AppSessionTogglePath, []string{"ctrl+p"}, "Toggle session path display"},
		{AppSessionToggleSort, []string{"ctrl+s"}, "Toggle session sort mode"},
		{AppSessionRename, []string{"ctrl+r"}, "Rename session"},
		{AppSessionDelete, []string{"ctrl+d"}, "Delete session"},
		{AppSessionDeleteNoninvasive, []string{"ctrl+backspace"}, "Delete session when query is empty"},
		{AppModelsSave, []string{"ctrl+s"}, "Save model selection"},
		{AppModelsEnableAll, []string{"ctrl+a"}, "Enable all models"},
		{AppModelsClearAll, []string{"ctrl+x"}, "Clear all models"},
		{AppModelsToggleProvider, []string{"ctrl+p"}, "Toggle all models for provider"},
		{AppModelsReorderUp, []string{"alt+up"}, "Move model up in order"},
		{AppModelsReorderDown, []string{"alt+down"}, "Move model down in order"},
		{AppTreeFilterDefault, []string{"ctrl+d"}, "Tree filter: default view"},
		{AppTreeFilterNoTools, []string{"ctrl+t"}, "Tree filter: hide tool results"},
		{AppTreeFilterUserOnly, []string{"ctrl+u"}, "Tree filter: user messages only"},
		{AppTreeFilterLabeledOnly, []string{"ctrl+l"}, "Tree filter: labeled entries only"},
		{AppTreeFilterAll, []string{"ctrl+a"}, "Tree filter: show all entries"},
		{AppTreeFilterCycleForward, []string{"ctrl+o"}, "Tree filter: cycle forward"},
		{AppTreeFilterCycleBackward, []string{"shift+ctrl+o"}, "Tree filter: cycle backward"},
	}
}
