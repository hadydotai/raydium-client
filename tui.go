package main

import (
	"fmt"
	"strconv"
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
	userDecisionBailout userDecision = iota
	userDecisionNOOP
	userDecisionProceed
	userDecisionReject
)

type promptKind uint8

const (
	promptKindIntent promptKind = iota
	promptKindSlippage
)

var spinnerFrames = []rune{'|', '/', '-', '\\'}

type renderResult struct {
	intentMeta *IntentInstruction
	table      string
	err        error
}

type termUI struct {
	builder         *TableBuilder
	resultCh        chan renderResult
	done            chan struct{}
	mode            uiMode
	promptBuffer    []rune
	promptKind      promptKind
	tableLines      []string
	busy            bool
	busyIntent      string
	currentIntent   string
	intentInput     string
	spinnerFrame    int
	statusMessage   string
	intentMeta      *IntentInstruction
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

func (ui *termUI) Run(initialIntent string) (*IntentInstruction, error) {
	if err := termbox.Init(); err != nil {
		return nil, err
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
				return nil, ev.Err
			case termbox.EventResize:
				continue
			case termbox.EventKey:
				if decision, ok := ui.handleKey(ev); ok {
					switch decision {
					case userDecisionNOOP:
						// NOTE(@hadydotai): Yeah TUIs man. So okay, we can't be here. Period.
						// while I guess it's harmless, it just shouldn't happen.
						//
						// userDecisionNOOP is a sentinel value, which means it needs to come with an ok == false. We also could technically
						// do a `continue` here, which is exactly what we want really, but I'd rather not. Rather panic
						// and fix the wrong semantic source than cover for it here defensively.
						panic("we shouldn't be here, NOOP + true means something went wrong in handleKey")
					case userDecisionReject, userDecisionBailout:
						return nil, nil
					default:
						return ui.intentMeta, nil
					}
				}
			}
		case res := <-ui.resultCh:
			ui.busy = false
			ui.spinnerFrame = 0
			ui.intentMeta = res.intentMeta
			if res.intentMeta != nil {
				ui.currentIntent = res.intentMeta.String()
			}
			if res.err != nil {
				ui.statusMessage = fmt.Sprintf("failed to compute intent: %v", res.err)
				ui.mode = modeAwaitDecision
			} else {
				ui.tableLines = splitLines(res.table)
				ui.statusMessage = "Press y=yes, n=no, c=change intent, s=slippage."
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
	ui.intentInput = intent
	ui.spinnerFrame = 0
	ui.statusMessage = ""
	go func(intent string) {
		tableStr, intentMeta, err := ui.builder.Build(intent)
		select {
		case ui.resultCh <- renderResult{intentMeta: intentMeta, table: tableStr, err: err}:
		case <-ui.done:
		}
	}(intent)
}

func (ui *termUI) handleKey(ev termbox.Event) (userDecision, bool) {
	if ev.Key == termbox.KeyCtrlC {
		return userDecisionBailout, true
	}
	switch ui.mode {
	case modeBusy:
		if ev.Key == termbox.KeyEsc {
			return userDecisionBailout, true
		}
	case modeAwaitDecision:
		switch ev.Ch {
		case 'y', 'Y':
			return userDecisionProceed, true
		case 'n', 'N':
			return userDecisionReject, true
		case 'c', 'C':
			ui.mode = modePrompt
			ui.promptBuffer = ui.promptBuffer[:0]
			ui.statusMessage = "Enter a new intent (<verb> <amount> <token-symbol>) and press Enter."
			ui.cursorVisible = true
			ui.promptKind = promptKindIntent
		case 's', 'S':
			ui.mode = modePrompt
			ui.promptBuffer = ui.promptBuffer[:0]
			ui.statusMessage = "Enter slippage percent (e.g. 0.5) and press Enter."
			ui.cursorVisible = true
			ui.promptKind = promptKindSlippage
		}
		if ev.Key == termbox.KeyEsc {
			return userDecisionReject, true
		}
	case modePrompt:
		switch ev.Key {
		case termbox.KeyEnter:
			value := strings.TrimSpace(string(ui.promptBuffer))
			switch ui.promptKind {
			case promptKindIntent:
				if value == "" {
					ui.statusMessage = "Intent cannot be empty."
					return userDecisionNOOP, false
				}
				ui.promptBuffer = ui.promptBuffer[:0]
				ui.startCompute(value)
				return userDecisionNOOP, false
			case promptKindSlippage:
				if value == "" {
					ui.statusMessage = "Slippage cannot be empty."
					return userDecisionNOOP, false
				}
				parsed, err := strconv.ParseFloat(value, 64)
				if err != nil {
					ui.statusMessage = fmt.Sprintf("invalid slippage: %v", err)
					return userDecisionNOOP, false
				}
				if err := ui.builder.SetSlippagePct(parsed); err != nil {
					ui.statusMessage = err.Error()
					return userDecisionNOOP, false
				}
				ui.promptBuffer = ui.promptBuffer[:0]
				ui.mode = modeBusy
				intent := ui.intentInput
				if intent == "" {
					intent = ui.currentIntent
				}
				if strings.TrimSpace(intent) == "" {
					intent = "pay 100"
				}
				ui.startCompute(intent)
				return userDecisionNOOP, false
			}
		case termbox.KeyEsc:
			ui.mode = modeAwaitDecision
			ui.promptBuffer = ui.promptBuffer[:0]
			ui.statusMessage = "Press y=yes, n=no, c=change intent, s=slippage."
			ui.cursorVisible = true
			return userDecisionNOOP, false
		case termbox.KeyBackspace, termbox.KeyBackspace2:
			if len(ui.promptBuffer) > 0 {
				ui.promptBuffer = ui.promptBuffer[:len(ui.promptBuffer)-1]
			}
			return userDecisionNOOP, false
		}
		if ev.Ch != 0 {
			ui.promptBuffer = append(ui.promptBuffer, ev.Ch)
		} else if ev.Key == termbox.KeySpace {
			ui.promptBuffer = append(ui.promptBuffer, ' ')
		}
	}
	return userDecisionNOOP, false
}

func (ui *termUI) draw() {
	termbox.Clear(termbox.ColorDefault, termbox.ColorDefault)
	width, height := termbox.Size()
	tableArea := max(height-2, 0)
	linesToShow := min(len(ui.tableLines), tableArea)
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
	for i := range linesToShow {
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
	return "Press y=yes, n=no, c=change intent, s=slippage."
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
