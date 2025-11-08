package main

import (
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/nsf/termbox-go"
)

type uiMode uint8

const (
	modeBusy uiMode = iota
	modeAwaitDecision
	modePrompt
)

type userDecision uint8

const (
	decisionNone userDecision = iota
	decisionProceed
	decisionDecline
	decisionAbort
)

var spinnerFrames = []rune{'|', '/', '-', '\\'}

type renderResult struct {
	intent string
	table  string
	err    error
}

type termUI struct {
	builder         *TableBuilder
	resultCh        chan renderResult
	done            chan struct{}
	mode            uiMode
	promptBuffer    []rune
	tableLines      []string
	busy            bool
	busyIntent      string
	currentIntent   string
	spinnerFrame    int
	statusMessage   string
	decision        userDecision
	cursorVisible   bool
	tableFlashUntil time.Time
}

func newTermUI(builder *TableBuilder) *termUI {
	return &termUI{
		builder:       builder,
		resultCh:      make(chan renderResult),
		done:          make(chan struct{}),
		cursorVisible: true,
	}
}

func (ui *termUI) Run(initialIntent string) (userDecision, error) {
	if err := termbox.Init(); err != nil {
		return decisionNone, err
	}
	defer termbox.Close()
	defer close(ui.done)
	eventCh := make(chan termbox.Event)
	go func() {
		for {
			eventCh <- termbox.PollEvent()
		}
	}()
	ticker := time.NewTicker(120 * time.Millisecond)
	defer ticker.Stop()

	ui.startCompute(initialIntent)
	for {
		ui.draw()
		select {
		case ev := <-eventCh:
			switch ev.Type {
			case termbox.EventError:
				return decisionNone, ev.Err
			case termbox.EventResize:
				continue
			case termbox.EventKey:
				if ui.handleKey(ev) {
					return ui.decision, nil
				}
			}
		case res := <-ui.resultCh:
			ui.busy = false
			ui.spinnerFrame = 0
			ui.currentIntent = res.intent
			if res.err != nil {
				ui.statusMessage = fmt.Sprintf("failed to compute intent: %v", res.err)
				ui.mode = modeAwaitDecision
			} else {
				ui.tableLines = splitLines(res.table)
				ui.statusMessage = "Press y=yes, n=no, c=change."
				ui.tableFlashUntil = time.Now().Add(350 * time.Millisecond)
				ui.mode = modeAwaitDecision
			}
		case <-ticker.C:
			if ui.busy {
				ui.spinnerFrame = (ui.spinnerFrame + 1) % len(spinnerFrames)
			}
			if ui.mode == modePrompt {
				ui.cursorVisible = !ui.cursorVisible
			} else {
				ui.cursorVisible = true
			}
		}
	}
}

func (ui *termUI) startCompute(intent string) {
	ui.busy = true
	ui.mode = modeBusy
	ui.busyIntent = intent
	ui.spinnerFrame = 0
	ui.statusMessage = ""
	go func(intent string) {
		tableStr, err := ui.builder.Build(intent)
		select {
		case ui.resultCh <- renderResult{intent: intent, table: tableStr, err: err}:
		case <-ui.done:
		}
	}(intent)
}

func (ui *termUI) handleKey(ev termbox.Event) bool {
	if ev.Key == termbox.KeyCtrlC {
		ui.decision = decisionAbort
		return true
	}
	switch ui.mode {
	case modeBusy:
		if ev.Key == termbox.KeyEsc {
			ui.decision = decisionAbort
			return true
		}
	case modeAwaitDecision:
		switch ev.Ch {
		case 'y', 'Y':
			ui.decision = decisionProceed
			return true
		case 'n', 'N':
			ui.decision = decisionDecline
			return true
		case 'c', 'C':
			ui.mode = modePrompt
			ui.promptBuffer = ui.promptBuffer[:0]
			ui.statusMessage = "Enter a new intent and press Enter."
			ui.cursorVisible = true
		}
		if ev.Key == termbox.KeyEsc {
			ui.decision = decisionDecline
			return true
		}
	case modePrompt:
		switch ev.Key {
		case termbox.KeyEnter:
			intent := strings.TrimSpace(string(ui.promptBuffer))
			if intent == "" {
				ui.statusMessage = "Intent cannot be empty."
				return false
			}
			ui.promptBuffer = ui.promptBuffer[:0]
			ui.startCompute(intent)
			return false
		case termbox.KeyEsc:
			ui.mode = modeAwaitDecision
			ui.promptBuffer = ui.promptBuffer[:0]
			ui.statusMessage = "Press y=yes, n=no, c=change."
			ui.cursorVisible = true
			return false
		case termbox.KeyBackspace, termbox.KeyBackspace2:
			if len(ui.promptBuffer) > 0 {
				ui.promptBuffer = ui.promptBuffer[:len(ui.promptBuffer)-1]
			}
			return false
		}
		if ev.Ch != 0 {
			ui.promptBuffer = append(ui.promptBuffer, ev.Ch)
		} else if ev.Key == termbox.KeySpace {
			ui.promptBuffer = append(ui.promptBuffer, ' ')
		}
	}
	return false
}

func (ui *termUI) draw() {
	termbox.Clear(termbox.ColorDefault, termbox.ColorDefault)
	width, height := termbox.Size()
	tableArea := height - 2
	if tableArea < 0 {
		tableArea = 0
	}
	linesToShow := len(ui.tableLines)
	if linesToShow > tableArea {
		linesToShow = tableArea
	}
	startRow := 0
	if linesToShow < tableArea {
		startRow = tableArea - linesToShow
	}
	fg := termbox.ColorDefault
	bg := termbox.ColorDefault
	flashActive := time.Now().Before(ui.tableFlashUntil)
	if flashActive {
		fg = termbox.ColorWhite | termbox.AttrBold
		bg = termbox.ColorGreen
	}
	for i := 0; i < linesToShow; i++ {
		ui.drawTextColor(0, startRow+i, width, ui.tableLines[i], fg, bg)
	}
	if height >= 2 {
		ui.drawText(0, height-2, width, ui.statusLine())
	}
	if height >= 1 {
		ui.drawText(0, height-1, width, ui.promptLine())
		ui.drawCursor(width, height-1)
	}
	termbox.Flush()
}

func (ui *termUI) drawText(x, y, width int, text string) {
	ui.drawTextColor(x, y, width, text, termbox.ColorDefault, termbox.ColorDefault)
}

func (ui *termUI) drawTextColor(x, y, width int, text string, fg, bg termbox.Attribute) {
	if y < 0 {
		return
	}
	col := 0
	for _, ch := range text {
		if col >= width {
			break
		}
		termbox.SetCell(x+col, y, ch, fg, bg)
		col++
	}
}

func (ui *termUI) statusLine() string {
	if ui.busy {
		frame := spinnerFrames[ui.spinnerFrame%len(spinnerFrames)]
		return fmt.Sprintf("%c computing intent %q", frame, ui.busyIntent)
	}
	if ui.statusMessage != "" {
		return ui.statusMessage
	}
	if ui.mode == modePrompt {
		return "Enter a new intent and press Enter."
	}
	return "Press y=yes, n=no, c=change."
}

func (ui *termUI) promptLine() string {
	switch ui.mode {
	case modePrompt:
		return "> " + string(ui.promptBuffer)
	default:
		if ui.busy {
			return "> ..."
		}
		if ui.currentIntent != "" {
			return fmt.Sprintf("> current intent: %s", ui.currentIntent)
		}
		return "> press c to enter a new intent"
	}
}

func splitLines(s string) []string {
	s = strings.TrimRight(s, "\n")
	if s == "" {
		return nil
	}
	return strings.Split(s, "\n")
}

func (ui *termUI) drawCursor(width, row int) {
	if row < 0 || width <= 0 {
		return
	}
	if ui.mode != modePrompt {
		return
	}
	col := ui.promptCursorColumn()
	if col >= width {
		col = width - 1
	}
	if col < 0 {
		return
	}
	ch := ' '
	if ui.cursorVisible {
		ch = '_'
	}
	termbox.SetCell(col, row, ch, termbox.ColorDefault, termbox.ColorDefault)
}

func (ui *termUI) promptCursorColumn() int {
	if ui.mode != modePrompt {
		return -1
	}
	return utf8.RuneCountInString("> " + string(ui.promptBuffer))
}
