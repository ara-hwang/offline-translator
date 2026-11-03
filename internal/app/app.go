// Package app wires the mygo window, tray, engine supervision, downloads,
// hotkeys and capture together: the UI shows state and fires events, the
// logic lives in internal/core.
package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/egoist/mygo"
	"github.com/egoist/mygo/ui"
	"offline-translator/internal/core"
	"offline-translator/internal/winapi"
)

// CopyTranslation copies the current output without changing its whitespace.
func (a *App) CopyTranslation() {
	if strings.TrimSpace(a.OutputText) == "" {
		a.Mu.Lock()
		a.Notice = "복사할 번역 결과가 없습니다."
		a.Mu.Unlock()
		return
	}
	err := winapi.CopyText(a.OutputText)
	a.Mu.Lock()
	defer a.Mu.Unlock()
	if err != nil {
		a.Notice = fmt.Sprintf("번역 결과를 복사하지 못했습니다: %v", err)
		return
	}
	a.Notice = "번역 결과를 복사했습니다."
}

// UI tab indexes.
const (
	tabTranslate = 0
	tabModels    = 1
	tabSettings  = 2
)

// autoSourceLabel is the source select's detect-language entry.
const autoSourceLabel = "자동"

// App is the shared state the UI reads and mutates. Fields written from
// goroutines are guarded by Mu; the view runs on the main thread.
type App struct {
	paths    core.AppPaths
	settings core.Settings

	Mu                sync.Mutex
	Tab               int
	SourceText        string
	OutputText        string
	OutputStats       string
	Streaming         bool
	EngineState       core.EngineState
	EngineLinesRaw    []string
	ModelFiles        []string
	DownloadID        string
	Download          core.DownloadProgress
	Notice            string
	SourceSelName     string
	TargetSelName     string
	AppearanceSel     string
	PromptLanguageSel string
	HotkeyRecording   bool

	winMu      sync.Mutex
	win        *mygo.Window
	invalidate func()

	supervisor    *core.Supervisor
	hotkeyReq     string
	appearanceIdx int
	cancelMu      sync.Mutex
	translateC    context.CancelFunc
	downloadC     context.CancelFunc
}

// New creates the application state and loads settings.
func New() *App {
	a := &App{}
	a.paths = core.DetectPaths()
	_ = a.paths.Ensure()
	a.settings, a.Notice = core.LoadOrDefault(a.paths.SettingsFile())
	// AutoSource keeps the source select on 자동 across restarts; its stored
	// concrete language stays as the detect-failure fallback.
	if a.settings.AutoSource {
		a.SourceSelName = autoSourceLabel
	} else {
		a.SourceSelName = languageName(a.settings.SourceLang)
	}
	a.TargetSelName = languageName(a.settings.TargetLang)
	a.AppearanceSel = appearanceName(a.settings.Appearance)
	a.PromptLanguageSel = promptLanguageName(a.settings.Prompt.Language)
	return a
}

// Invalidate refreshes the window from any goroutine.
func (a *App) Invalidate() {
	a.winMu.Lock()
	invalidate := a.invalidate
	a.winMu.Unlock()
	if invalidate != nil {
		invalidate()
	}
}

// noticeLock is a compile-time assertion the notice lives behind Mu.
func (a *App) noticeLock() { a.Mu.Lock(); a.Mu.Unlock() }

// themeSource maps the stored appearance onto mygo's theme source.
func (a *App) themeSource() mygo.ThemeSource {
	switch a.settings.Appearance {
	case core.AppearanceLight:
		return mygo.ThemeLight
	case core.AppearanceDark:
		return mygo.ThemeDark
	default:
		return mygo.ThemeSystem
	}
}

// languageIndexOf resolves a code into the language table index.
func languageIndexOf(code string) int {
	for i, lang := range core.Languages {
		if lang.Code == code {
			return i
		}
	}
	return 1 // en
}

// languageName maps a settings code onto the select label used in the UI.
func languageName(code string) string {
	return core.Languages[languageIndexOf(code)].NameKo
}

// appearanceName maps the stored appearance onto the select label.
func appearanceName(a core.Appearance) string {
	switch a {
	case core.AppearanceLight:
		return "라이트"
	case core.AppearanceDark:
		return "다크"
	default:
		return "시스템"
	}
}

// syncLanguageSelects copies the select names back into the settings. The
// 자동 entry only flips AutoSource: the stored source language stays as the
// detect-failure fallback.
func (a *App) syncLanguageSelects() {
	a.Mu.Lock()
	target := languageCodeByName(a.TargetSelName)
	if a.SourceSelName == autoSourceLabel {
		a.settings.AutoSource = true
	} else if source := languageCodeByName(a.SourceSelName); source != "" {
		a.settings.AutoSource = false
		a.settings.SourceLang = source
	}
	if target != "" && target != a.settings.SourceLang {
		a.settings.TargetLang = target
	}
	settings := a.settings
	a.Mu.Unlock()
	go settings.Save(a.paths.SettingsFile())
}

// languageCodeByName resolves a select label back to a stable code.
func languageCodeByName(name string) string {
	for _, lang := range core.Languages {
		if lang.NameKo == name {
			return lang.Code
		}
	}
	return ""
}

// SaveSettings persists the current settings asynchronously and re-applies
// desktop behavior and hotkeys.
func (a *App) SaveSettings() {
	a.Mu.Lock()
	settings := a.settings
	a.Mu.Unlock()
	go func() {
		if err := settings.Save(a.paths.SettingsFile()); err != nil {
			a.Mu.Lock()
			a.settingsMsgLocked(fmt.Sprintf("설정을 저장하지 못했습니다: %v", err))
			a.Mu.Unlock()
			a.Invalidate()
		}
	}()
	a.applyDesktop()
	a.applyHotkey()
	a.Invalidate()
}

// settingsMsgLocked sets Notice; caller holds or re-locks Mu.
func (a *App) settingsMsgLocked(msg string) { a.Notice = msg }

func (a *App) applyDesktop() {
	if a.settings.Desktop.StartWithWindows != mygo.App.OpenAtLogin() {
		_ = mygo.App.SetOpenAtLogin(a.settings.Desktop.StartWithWindows)
	}
}

// applyHotkey (re)registers the configured global shortcut.
func (a *App) applyHotkey() {
	if a.hotkeyReq != "" {
		mygo.GlobalShortcut.Unregister(a.hotkeyReq)
		a.hotkeyReq = ""
	}
	if !a.settings.Hotkey.Enabled {
		return
	}
	combo := core.DefaultHotkeyCombo()
	if a.settings.Hotkey.Combo != nil && a.settings.Hotkey.Combo.IsValid() {
		combo = *a.settings.Hotkey.Combo
	} else if a.settings.Hotkey.Combo != nil {
		a.Mu.Lock()
		a.Notice = "단축키 조합이 유효하지 않습니다. 설정 파일을 확인하세요."
		a.Mu.Unlock()
		return
	}
	accel := comboToAccelerator(combo)
	if err := mygo.GlobalShortcut.Register(accel, func() {
		go a.captureSelection()
	}); err != nil {
		a.Mu.Lock()
		a.Notice = fmt.Sprintf("단축키를 등록하지 못했습니다(%s): %v", accel, err)
		a.Mu.Unlock()
		a.Invalidate()
		return
	}
	a.hotkeyReq = accel
}

// comboToAccelerator renders a combo as a mygo accelerator string.
func comboToAccelerator(combo core.HotkeyCombo) string {
	var b strings.Builder
	if combo.Ctrl {
		b.WriteString("Ctrl+")
	}
	if combo.Alt {
		b.WriteString("Alt+")
	}
	if combo.Shift {
		b.WriteString("Shift+")
	}
	if combo.Win {
		b.WriteString("Win+")
	}
	key := core.NormalizeHotkeyKey(combo.Key)
	switch key {
	case "space":
		b.WriteString("Space")
	case "tab":
		b.WriteString("Tab")
	case "enter":
		b.WriteString("Enter")
	default:
		b.WriteString(strings.ToUpper(key))
	}
	return b.String()
}

// hotkeyCombo returns the configured chord, or the default when none is
// stored yet.
func (a *App) hotkeyCombo() core.HotkeyCombo {
	if combo := a.settings.Hotkey.Combo; combo != nil && combo.IsValid() {
		return *combo
	}
	return core.DefaultHotkeyCombo()
}

// SwapLanguages exchanges the source and target languages. With the source
// select on 자동 the direction is resolved from the source text first, since
// auto cannot move to the target side: the resolved pair becomes the newly
// stored manual selection.
func (a *App) SwapLanguages() {
	a.Mu.Lock()
	sourceCode := a.settings.SourceLang
	if a.SourceSelName == autoSourceLabel {
		if lang, ok := core.Detect(a.SourceText); ok {
			sourceCode = lang.Code
		}
		a.settings.AutoSource = false
	}
	a.settings.SourceLang, a.settings.TargetLang = a.settings.TargetLang, sourceCode
	a.SourceSelName = languageName(a.settings.SourceLang)
	a.TargetSelName = languageName(a.settings.TargetLang)
	a.Mu.Unlock()
	a.SaveSettings()
}

// captureSelection copies the selected text of the focused application into
// the source editor, shows the window and starts translating.
func (a *App) captureSelection() {
	text, err := winapi.CaptureSelected()
	if err != nil {
		a.Mu.Lock()
		a.Notice = err.Error()
		a.Mu.Unlock()
		a.Invalidate()
		return
	}
	a.Mu.Lock()
	a.SourceText = text
	a.OutputText = ""
	a.Tab = tabTranslate
	a.Mu.Unlock()
	if win := a.window(); win != nil {
		win.Show()
		win.Focus()
	}
	a.Invalidate()
	a.StartTranslate()
}

func (a *App) window() *mygo.Window {
	a.winMu.Lock()
	defer a.winMu.Unlock()
	return a.win
}

// StartEngine starts the managed server for the selected model.
func (a *App) StartEngine() {
	a.Mu.Lock()
	config := a.launchConfig()
	if config != nil {
		a.EngineState = core.EngineState{Kind: core.EngineStarting}
	}
	a.Mu.Unlock()
	if config == nil {
		a.Mu.Lock()
		a.EngineState = core.EngineState{Kind: core.EngineFailed,
			Detail: "사용할 수 있는 모델 없음"}
		a.Mu.Unlock()
		a.Invalidate()
		return
	}
	a.supervisor.Start(*config)
}

// launchConfig resolves the model file and builds the launch config.
func (a *App) launchConfig() *core.LaunchConfig {
	model := deref(a.settings.Server.ModelPath)
	if model != "" && !fileExists(model) {
		model = ""
	}
	if model == "" {
		model = core.PickModel(core.ScanGGUF(a.paths.ModelsDir()))
	}
	if model == "" {
		return nil
	}
	server := core.DiscoverServer(exeDir(), deref(a.settings.Server.RuntimePath))
	if server == "" {
		return nil
	}
	return &core.LaunchConfig{
		ServerPath: server,
		ModelPath:  model,
		Port:       a.settings.Server.Port,
		CtxSize:    a.settings.Server.CtxSize,
		Threads:    a.settings.Server.Threads,
		GPULayers:  a.settings.Server.GPULayers,
		Alias:      "hy-mt2",
	}
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

func fileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}

func exeDir() string {
	exe, err := os.Executable()
	if err != nil {
		return ""
	}
	return filepath.Dir(exe)
}

// StopEngine stops the managed server.
func (a *App) StopEngine() { a.supervisor.Stop() }

// StartDownload begins downloading a catalog model with resume support.
func (a *App) StartDownload(option core.ModelOption) {
	a.Mu.Lock()
	if a.DownloadID != "" {
		a.Mu.Unlock()
		return
	}
	a.DownloadID = option.ID
	a.Download = core.DownloadProgress{Total: option.SizeBytes}
	base := a.settings.DownloadBase
	dest := filepath.Join(a.paths.ModelsDir(), option.FileName())
	a.Mu.Unlock()
	a.Invalidate()

	go a.runDownload(option, option.DownloadURL(base), dest, core.PartPath(dest))
}

// runDownload streams the file, resuming from the .part file when present.
func (a *App) runDownload(option core.ModelOption, url, dest, part string) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	a.cancelMu.Lock()
	a.downloadC = cancel
	a.cancelMu.Unlock()

	progress := int64(0)
	if info, err := os.Stat(part); err == nil {
		progress = info.Size()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		a.finishDownload(fmt.Sprintf("다운로드를 시작하지 못했습니다: %v", err))
		return
	}
	if progress > 0 {
		req.Header.Set("Range", "bytes="+strconv.FormatInt(progress, 10)+"-")
	}
	resp, doErr := http.DefaultClient.Do(req)
	if doErr != nil {
		if ctx.Err() != nil {
			a.finishDownload("다운로드를 취소했습니다.")
		} else {
			a.finishDownload(fmt.Sprintf("다운로드를 시작하지 못했습니다: %v", doErr))
		}
		return
	}
	defer resp.Body.Close()
	if progress > 0 && resp.StatusCode != http.StatusPartialContent {
		progress = 0 // restart when the host cannot resume
	}
	total := progress + resp.ContentLength
	if resp.ContentLength <= 0 {
		total = option.SizeBytes
	}
	if total < progress {
		total = progress
	}

	flag := os.O_WRONLY | os.O_CREATE | os.O_TRUNC
	if progress > 0 {
		flag = os.O_WRONLY | os.O_CREATE | os.O_APPEND
	}
	out, err := os.OpenFile(part, flag, 0o644)
	if err != nil {
		a.finishDownload(fmt.Sprintf("파일을 열지 못했습니다: %v", err))
		return
	}

	buf := make([]byte, 256*1024)
	written := progress
	var lastInvalidate time.Time
	for {
		nn, readErr := resp.Body.Read(buf)
		if nn > 0 {
			if _, werr := out.Write(buf[:nn]); werr != nil {
				out.Close()
				a.finishDownload(fmt.Sprintf("파일에 쓰지 못했습니다: %v", werr))
				return
			}
			written += int64(nn)
			if time.Since(lastInvalidate) > 150*time.Millisecond {
				lastInvalidate = time.Now()
				a.Mu.Lock()
				a.Download = core.DownloadProgress{Downloaded: written, Total: total}
				a.Mu.Unlock()
				a.Invalidate()
			}
		}
		if ctx.Err() != nil {
			out.Close()
			os.Remove(part)
			a.Mu.Lock()
			a.Notice = "다운로드를 취소했습니다."
			a.Mu.Unlock()
			a.finishDownload("")
			return
		}
		if readErr != nil {
			out.Close()
			if !errors.Is(readErr, io.EOF) {
				a.finishDownload(fmt.Sprintf("다운로드 중 오류: %v", readErr))
				return
			}
			break
		}
	}
	if err := os.Rename(part, dest); err != nil {
		a.finishDownload(fmt.Sprintf("완성 파일을 옮기지 못했습니다: %v", err))
		return
	}
	a.refreshLocalModels()
	a.finishDownload("")
	mygo.NewNotification(mygo.NotificationOptions{
		Title: "다운로드 완료",
		Body:  option.FileName(),
	}).Show()
}

func (a *App) finishDownload(msg string) {
	a.Mu.Lock()
	a.DownloadID = ""
	a.Download = core.DownloadProgress{}
	if msg != "" {
		a.Notice = msg
		a.Tab = tabModels
	}
	a.Mu.Unlock()
	a.Invalidate()
}

// CancelDownload cancels the running download; the .part file is removed.
func (a *App) CancelDownload() {
	a.cancelMu.Lock()
	cancel := a.downloadC
	a.cancelMu.Unlock()
	if cancel != nil {
		cancel()
	}
}

// StartTranslate streams a translation into OutputText.
func (a *App) StartTranslate() {
	a.Mu.Lock()
	if a.Streaming {
		a.Mu.Unlock()
		return
	}
	source := a.SourceText
	targetCode := a.settings.TargetLang
	promptSettings := a.settings.Prompt
	generation := a.settings.Generation
	persist := false
	// With the source select on 자동 the direction is resolved from the text
	// before the prompt is built. On a detect failure the stored source and
	// target pair stays as it is.
	translateNotice := ""
	if a.SourceSelName == autoSourceLabel && strings.TrimSpace(source) != "" {
		if _, ok := core.Detect(source); !ok {
			translateNotice = "소스 언어를 인식하지 못했습니다. 원문 언어를 직접 선택해 주세요."
		} else {
			oldTarget := a.settings.TargetLang
			sourceCode, newTarget, swapped := core.ResolveDirection(source, a.settings.SourceLang, a.settings.TargetLang)
			if sourceCode != a.settings.SourceLang || newTarget != oldTarget {
				persist = true
			}
			a.settings.SourceLang = sourceCode
			a.settings.TargetLang = newTarget
			targetCode = newTarget
			if newTarget != oldTarget {
				a.TargetSelName = languageName(newTarget)
			}
			if swapped {
				translateNotice = "감지된 언어가 타깃과 같아 방향을 바꿉니다."
			}
		}
	}
	settings := a.settings
	a.Mu.Unlock()
	if persist {
		go settings.Save(a.paths.SettingsFile())
	}

	// An empty input is expected; the source editor's placeholder carries
	// the hint, not the notice bar.
	if strings.TrimSpace(source) == "" {
		return
	}

	if ok, message := core.ValidateSource(source); !ok {
		a.Mu.Lock()
		a.Notice = message
		a.Mu.Unlock()
		a.Invalidate()
		return
	}
	prompt := buildPrompt(promptSettings, source, targetCode)
	endpoint := core.LocalhostEndpoint(a.settings.Server.Port)
	const model = "hy-mt2"

	ctx, cancel := context.WithCancel(context.Background())
	a.cancelMu.Lock()
	a.translateC = cancel
	a.cancelMu.Unlock()

	a.Mu.Lock()
	a.Streaming = true
	a.OutputText = ""
	a.OutputStats = ""
	a.Notice = translateNotice
	a.Mu.Unlock()
	a.Invalidate()

	go func() {
		defer cancel()
		result, err := core.TranslateStream(ctx, endpoint, model, core.GenParamsFrom(generation), prompt, nil)
		a.Mu.Lock()
		a.Streaming = false
		a.OutputStats = ""
		if err != nil {
			if chat, ok := err.(*core.ChatError); ok {
				switch chat.Kind {
				case core.ChatErrorHTTP:
					if strings.Contains(chat.Error(), "connect") || strings.Contains(chat.Error(), "refused") {
						a.Notice = "모델 서버에 연결할 수 없습니다. 모델 탭에서 엔진을 시작하세요."
					} else {
						a.Notice = chat.Error()
					}
				default:
					a.Notice = chat.Error()
				}
			} else {
				a.Notice = err.Error()
			}
			if result != nil && result.Text != "" {
				a.OutputText = result.Text
			}
		} else if result != nil {
			a.OutputText = result.Text
			speed := ""
			if result.TokensPerSecond > 0 {
				speed = fmt.Sprintf(" · %.1f tok/s", result.TokensPerSecond)
			}
			a.OutputStats = fmt.Sprintf("%d ms · %d tok%s", result.TotalMs, result.CompletionTokens, speed)
		}
		a.Mu.Unlock()
		a.Invalidate()
	}()
}

// CancelTranslate cancels the running translation; partial text stays.
func (a *App) CancelTranslate() {
	a.cancelMu.Lock()
	cancel := a.translateC
	a.cancelMu.Unlock()
	if cancel != nil {
		cancel()
	}
}

// buildPrompt assembles the prompt from settings, keeping one exclusive
// mode priority as the Rust UI did: delimiters, then glossary, then style.
func buildPrompt(settings core.PromptSettings, source, targetCode string) string {
	target, ok := core.ByCode(targetCode)
	if !ok {
		target = core.DefaultTarget()
	}
	mode := core.PromptMode{Style: settings.Style}
	if settings.PreserveDelimiters {
		mode.Delimiters = true
		mode.Style = ""
	}
	if settings.UseGlossary && !mode.Delimiters {
		mode.Pairs = core.ParseGlossary(settings.Glossary)
	}
	language := core.PromptLanguage(settings.Language)
	return core.BuildPrompt(mode, source, target, language)
}

// engineEventSink publishes supervisor events into app state.
func (a *App) engineEventSink(event core.EngineEvent) {
	if event.State != nil {
		a.Mu.Lock()
		a.EngineState = *event.State
		a.Mu.Unlock()
	}
	if event.Log != "" {
		a.Mu.Lock()
		a.EngineLinesRaw = append(a.EngineLinesRaw, event.Log)
		if len(a.EngineLinesRaw) > 400 {
			a.EngineLinesRaw = a.EngineLinesRaw[len(a.EngineLinesRaw)-400:]
		}
		a.Mu.Unlock()
	}
	a.Invalidate()
}

// pumpEvents drains the supervisor channel.
func (a *App) pumpEvents() {
	for event := range a.supervisor.Events() {
		a.engineEventSink(event)
	}
}

// EngineLabel is the status bar engine text.
func (a *App) EngineLabel() string {
	a.Mu.Lock()
	defer a.Mu.Unlock()
	switch a.EngineState.Kind {
	case core.EngineStarting:
		return "모델 불러오는 중…"
	case core.EngineFailed:
		trimmed := strings.TrimSpace(a.EngineState.Detail)
		if trimmed == "" {
			return "오류"
		}
		runes := []rune(trimmed)
		if len(runes) > 60 {
			trimmed = string(runes[:60]) + "…"
		}
		return trimmed
	default:
		return a.EngineState.Label()
	}
}

// Run is the app entry: single instance lock, theme, window and tray.
func Run() {
	a := New()

	if !mygo.App.RequestSingleInstanceLock() {
		return
	}
	mygo.App.SetName("Offline Translator")
	mygo.App.SetVersion(core.Version)
	// Register the embedded font once, before the first frame; a failure
	// keeps the system font instead. See ui.go for the family's use.
	_ = registerAppFont()
	mygo.Theme.SetSource(a.themeSource())
	if a.settings.Desktop.StartWithWindows {
		_ = mygo.App.SetOpenAtLogin(true)
	}
	// The desktop toggles the Run key; settings windows later re-apply it.
	_ = a.settings.Desktop.StartWithWindows

	quitting := false
	mygo.App.OnBeforeQuit(func(*mygo.QuitEvent) { quitting = true })
	mygo.App.OnSecondInstance(func(args []string, cwd string) {
		if win := a.window(); win != nil {
			win.Restore()
			win.Focus()
		}
	})
	mygo.App.OnWindowAllClosed(func() {
		// Keep running with the tray while the user asked to stay resident.
		if !a.settings.Desktop.KeepInTray {
			mygo.App.Quit()
		}
	})

	supervisor := core.SpawnSupervisor(a.paths.EngineLog())
	a.supervisor = supervisor
	go a.pumpEvents()
	a.refreshLocalModels()

	mygo.App.WhenReady(func() {
		hidden := startedAtLogin(a)
		win := mygo.NewWindow(mygo.WindowOptions{
			Title:     "Offline Translator",
			Page:      mygo.PageOptions{DevTools: mygo.DevToolsDisabled},
			Width:     920,
			MinWidth:  700,
			Height:    640,
			MinHeight: 480,
			StateKey:  "main",
			Content:   ui.View(a.view),
			Hidden:    hidden,
		})
		a.winMu.Lock()
		a.win = win
		a.invalidate = win.Invalidate
		a.winMu.Unlock()
		_ = win.SetIcon(windowIconPNG)
		win.OnClose(func(e *mygo.CloseEvent) {
			if !quitting && a.settings.Desktop.KeepInTray {
				e.PreventDefault()
				win.Hide()
			}
		})
		if _, err := mygo.NewTray(mygo.TrayOptions{
			Icon:    trayIconPNG,
			ToolTip: "Offline Translator",
			Menu: mygo.NewMenu([]*mygo.MenuItem{
				{Label: "열기", Click: func(*mygo.MenuItem, *mygo.Window) {
					win.Show()
					win.Focus()
				}},
				mygo.Separator(),
				{Role: mygo.RoleQuit},
			}),
		}); err != nil {
			// Without a tray the desktop app behaves like a normal one.
			mygo.App.OnWindowAllClosed(func() {})
		}
		a.applyHotkey()
		if a.settings.Server.AutoStart {
			a.StartEngine()
		}
	})

	if err := mygo.App.Run(); err != nil {
		fmt.Println(err)
	}
	a.StopEngine()
}

// startedAtLogin hides the first window when a login launch asked for it.
func startedAtLogin(a *App) bool {
	if !mygo.App.WasOpenedAtLogin() {
		return false
	}
	return true
}

// textSize is the shared editor text size.
func (a *App) textSize() int {
	a.Mu.Lock()
	defer a.Mu.Unlock()
	return int(a.settings.TextSize)
}

// zoomText applies one text zoom step.
func (a *App) zoomText(zoom core.TextZoom) {
	a.Mu.Lock()
	a.settings.TextSize = core.FlexInt(core.ZoomTextSize(int(a.settings.TextSize), zoom))
	a.Mu.Unlock()
	a.SaveSettings()
}

// refreshLocalModels rescans the models folder.
func (a *App) refreshLocalModels() {
	files := core.ScanGGUF(a.paths.ModelsDir())
	a.Mu.Lock()
	a.ModelFiles = files
	a.Mu.Unlock()
}
