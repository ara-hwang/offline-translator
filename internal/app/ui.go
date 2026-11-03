package app

import (
	"fmt"
	"path/filepath"
	"strings"

	"github.com/egoist/mygo/ui"
	"offline-translator/internal/core"
)

// languageNames is the select option list.
func languageNames() []string {
	names := make([]string, len(core.Languages))
	for i, lang := range core.Languages {
		names[i] = lang.NameKo
	}
	return names
}

// sourceLanguageNames is the source select option list: the detect-language
// entry followed by every language.
func sourceLanguageNames() []string {
	names := make([]string, 0, len(core.Languages)+1)
	names = append(names, autoSourceLabel)
	for _, lang := range core.Languages {
		names = append(names, lang.NameKo)
	}
	return names
}

// appearanceNames is the appearance select option list.
var appearanceNames = []string{"시스템", "라이트", "다크"}

// promptLanguageNames is the instruction-language select option list.
var promptLanguageNames = []string{"자동", "영어", "중국어"}

// actionInset aligns the bottom-right actions across tabs.
const actionInset = 12

// appFontFamily is the family of the embedded Pretendard Variable the
// whole frame takes.
const appFontFamily = "Pretendard Variable"

// applyAppFont makes the frame build with the embedded Pretendard
// Variable: widgets created after the call take its font, where they set
// none of their own.
func applyAppFont(c *ui.Context) {
	t := *c.Theme()
	t.Font = appFontFamily
	c.SetTheme(&t)
}

// view is the root of the whole window: a tab row and the active tab surface.
func (a *App) view(c *ui.Context) {
	applyAppFont(c)
	a.noticeLock()
	a.Mu.Lock()
	tab := a.Tab
	notice := a.Notice
	a.Notice = ""
	a.Mu.Unlock()
	if strings.TrimSpace(notice) != "" {
		c.Toast(notice)
	}
	if a.HotkeyRecording && tab != tabSettings {
		a.cancelHotkeyRecording()
	}

	ui.Column(c).Fill().Children(func() {
		a.navBar(c)
		switch tab {
		case tabTranslate:
			a.translateView(c)
		case tabModels:
			a.modelsView(c)
		case tabSettings:
			a.settingsView(c)
		}
	})
}

// navBar puts the engine state at the right end of the tab row.
func (a *App) navBar(c *ui.Context) {
	ui.Row(c).PaddingX(10).Gap(8).AlignItems(ui.Center).Children(func() {
		ui.Tabs(c, &a.Tab, "번역", "모델", "설정")
		ui.Box(c).Grow(1) // Spacer pushing the engine state right.
		state := a.engineStateView()
		ui.Row(c).Gap(4).AlignItems(ui.Center).Children(func() {
			ui.Text(c, "●").FontSize(11).TextColor(engineStatusColor(state.Kind, c.Theme()))
			ui.Text(c, a.EngineLabel()).FontSize(11)
		})
		if a.isStreaming() {
			ui.Spinner(c)
			ui.Text(c, "번역 중").FontSize(11)
		}
		if a.isDownloading() {
			ui.Spinner(c)
			ui.Text(c, a.progress().Label()).FontSize(11)
		}
	})
}

// engineStatusColor uses theme colors so the indicator remains legible in both themes.
func engineStatusColor(kind core.EngineStateKind, theme *ui.Theme) ui.Color {
	switch kind {
	case core.EngineReady:
		return theme.Success
	case core.EngineStarting:
		return theme.Warning
	case core.EngineFailed:
		return theme.Danger
	default:
		return theme.TextMuted
	}
}

func (a *App) isStreaming() bool {
	a.Mu.Lock()
	defer a.Mu.Unlock()
	return a.Streaming
}

func (a *App) isDownloading() bool {
	a.Mu.Lock()
	defer a.Mu.Unlock()
	return a.DownloadID != ""
}

func (a *App) progress() core.DownloadProgress {
	a.Mu.Lock()
	defer a.Mu.Unlock()
	return a.Download
}

// translateView is the main working surface: the language pair and the
// text size on one bar, the source and translation editors side by side,
// and the action row.
func (a *App) translateView(c *ui.Context) {
	ui.Column(c).Fill().Padding(actionInset).Gap(8).Children(func() {
		ui.Row(c).Gap(8).AlignItems(ui.Center).Children(func() {
			ui.Select(c, &a.SourceSelName, sourceLanguageNames()).Width(150).OnChange(a.syncLanguageSelects)
			if ui.Button(c, "→").Clicked() {
				a.SwapLanguages()
			}
			ui.Select(c, &a.TargetSelName, languageNames()).Width(190).OnChange(a.syncLanguageSelects)
			ui.Box(c).Grow(1) // Spacer pushing the text size controls right.
			ui.Text(c, "글자 크기").FontSize(11)
			if ui.Button(c, "A−").Clicked() {
				a.zoomText(core.ZoomOut)
			}
			ui.Text(c, fmt.Sprintf("%d px", a.textSize())).FontSize(12)
			if ui.Button(c, "A+").Clicked() {
				a.zoomText(core.ZoomIn)
			}
			if ui.Button(c, "초기화").Clicked() {
				a.zoomText(core.ZoomReset)
			}
		})
		// The two editors sit side by side: Grow(1) on both of their columns
		// splits the row evenly whatever the window width is. Rows center
		// their children vertically by default, so stretch the columns to
		// make both editors fill the height.
		ui.Row(c).Fill().Grow(1).Gap(8).AlignItems(ui.Stretch).Children(func() {
			ui.Column(c).Grow(1).Gap(4).Children(func() {
				ui.Text(c, "원문").Bold().FontSize(11)
				ui.TextArea(c, &a.SourceText).
					Placeholder("번역할 텍스트를 입력하세요…").
					FontSize(float32(a.textSize())).
					Grow(1)
			})
			ui.Column(c).Grow(1).Gap(4).Children(func() {
				ui.Text(c, "번역").Bold().FontSize(11)
				ui.TextArea(c, &a.OutputText).FontSize(float32(a.textSize())).Grow(1)
			})
		})
		// The primary action sits at the right end of the row: the spacer
		// takes all free width and pushes the stats and button there.
		ui.Row(c).Gap(8).AlignItems(ui.Center).Children(func() {
			if ui.Button(c, "결과 복사").Clicked() {
				a.CopyTranslation()
			}
			ui.Box(c).Grow(1)
			if stats := a.statsText(); stats != "" {
				ui.Text(c, stats).FontSize(11)
			}
			if a.isStreaming() {
				if ui.Button(c, "중지").Clicked() {
					a.CancelTranslate()
				}
			} else if ui.PrimaryButton(c, "번역").Clicked() {
				a.StartTranslate()
			}
		})
	})
}

// modelsView groups the engine controls and downloadable models in a scrollable page.
func (a *App) modelsView(c *ui.Context) {
	theme := c.Theme()
	ui.Scroll(c).Fill().Padding(16).Gap(12).Children(func() {
		ui.Column(c).Gap(4).Children(func() {
			ui.Text(c, "모델 관리").FontSize(18).Bold()
		})

		state := a.engineStateView()
		ui.Column(c).Padding(12).Gap(8).Radius(8).Border(1, theme.Border).Background(theme.Surface).Children(func() {
			ui.Row(c).Gap(8).AlignItems(ui.Center).Children(func() {
				ui.Text(c, "번역 엔진").Bold()
				ui.Text(c, state.Label()).FontSize(11).TextColor(theme.TextMuted)
				ui.Box(c).Grow(1)
				if state.IsReady() || state.IsBusy() {
					if ui.Button(c, "엔진 중지").Clicked() {
						a.StopEngine()
					}
				} else if ui.PrimaryButton(c, "엔진 시작").Clicked() {
					a.StartEngine()
				}
			})
			model := a.selectedModelShort()
			if model == "" {
				model = "선택된 모델 없음"
			}
			ui.Text(c, "현재 모델 · "+model).FontSize(11).TextColor(theme.TextMuted)
			if detail, ok := a.engineFailure(); ok {
				ui.Text(c, detail).FontSize(11).TextColor(theme.Danger)
			}
			if tail := a.engineLogTail(); tail != "" {
				ui.Text(c, "서버 로그").FontSize(11).Bold()
				log := tail
				ui.TextArea(c, &log).Height(100)
			}
		})

		ui.Column(c).Gap(4).Children(func() {
			ui.Text(c, "공식 모델 카탈로그").Bold()
			ui.Text(c, "Tencent Hy-MT2 GGUF · 모델 크기와 권장 사양을 확인하세요.").FontSize(11).TextColor(theme.TextMuted)
		})
		for _, option := range core.Catalog {
			installed := a.installedOption(option)
			a.Mu.Lock()
			downloading := a.DownloadID == option.ID
			a.Mu.Unlock()
			ui.Column(c.Key(option.ID)).Padding(12).Gap(6).Radius(8).Border(1, theme.Border).Children(func() {
				ui.Row(c).Gap(8).AlignItems(ui.Center).Children(func() {
					ui.Text(c, option.FileName()).FontSize(12).Bold().Grow(1).MinWidth(0)
					if option.Recommended {
						ui.Text(c, "권장").FontSize(11).TextColor(theme.Accent)
					}
					ui.Text(c, option.SizeLabel()).FontSize(11).TextColor(theme.TextMuted)
					switch {
					case downloading:
						if ui.Button(c, "취소").Clicked() {
							a.CancelDownload()
						}
					case installed:
						ui.Text(c, "설치됨").FontSize(11).TextColor(theme.Success)
					case a.isDownloading():
						ui.Text(c, "대기 중").FontSize(11).TextColor(theme.TextMuted)
					default:
						if ui.Button(c, "내려받기").Clicked() {
							a.StartDownload(option)
						}
					}
				})
				ui.Text(c, option.Note).FontSize(11).TextColor(theme.TextMuted)
				if downloading {
					progress := a.progress()
					ui.Progress(c, progress.Fraction()).FillWidth()
					ui.Text(c, progress.Label()).FontSize(11).TextColor(theme.TextMuted)
				}
			})
		}
	})
}

// settingsView is the preferences surface. Checkboxes bind settings fields
// directly; the 저장 button persists and re-applies them.
func (a *App) settingsView(c *ui.Context) {
	a.Mu.Lock()
	if a.AppearanceSel == "" {
		a.AppearanceSel = appearanceName(a.settings.Appearance)
	}
	keepInTray := &a.settings.Desktop.KeepInTray
	startWithWindows := &a.settings.Desktop.StartWithWindows
	hotkeyEnabled := &a.settings.Hotkey.Enabled
	delimiters := &a.settings.Prompt.PreserveDelimiters
	useGlossary := &a.settings.Prompt.UseGlossary
	glossary := &a.settings.Prompt.Glossary
	style := &a.settings.Prompt.Style
	a.Mu.Unlock()

	theme := c.Theme()
	ui.Column(c).Fill().Children(func() {
		ui.Scroll(c).Grow(1).Padding(16).Gap(12).Children(func() {
			ui.Column(c).Gap(4).Children(func() {
				ui.Text(c, "설정").FontSize(18).Bold()
			})
			ui.Column(c).Padding(14).Gap(10).Radius(8).Border(1, theme.Border).Background(theme.Surface).Children(func() {
				ui.Text(c, "화면 및 시작").Bold()
				ui.Row(c).Gap(12).AlignItems(ui.Center).Children(func() {
					ui.Text(c, "테마").FontSize(12)
					ui.Select(c, &a.AppearanceSel, appearanceNames).Width(160)
				})
				ui.Checkbox(c, keepInTray, "창을 닫아도 트레이에 유지")
				ui.Checkbox(c, startWithWindows, "Windows 시작 시 자동 실행")
			})
			ui.Column(c).Padding(14).Gap(10).Radius(8).Border(1, theme.Border).Background(theme.Surface).Children(func() {
				ui.Text(c, "번역 지시문").Bold()
				ui.Row(c).Gap(12).AlignItems(ui.Center).Children(func() {
					ui.Text(c, "지시문 언어").FontSize(12)
					ui.Select(c, &a.PromptLanguageSel, promptLanguageNames).Width(160)
				})
				ui.Checkbox(c, delimiters, "구분자(코드 등) 원형 보존 강제")
				ui.Checkbox(c, useGlossary, "용어집 사용")
				if *useGlossary {
					ui.Text(c, "용어집 · 한 줄에 원문 => 번역 하나씩 입력").FontSize(11).TextColor(theme.TextMuted)
					ui.TextArea(c, glossary).Height(110)
					ui.Text(c, "스타일 지시문 (선택)").FontSize(11).TextColor(theme.TextMuted)
					ui.TextArea(c, style).Height(60)
				}
			})
			ui.Column(c).Padding(14).Gap(10).Radius(8).Border(1, theme.Border).Background(theme.Surface).Children(func() {
				ui.Text(c, "글로벌 단축키").Bold()
				ui.Checkbox(c, hotkeyEnabled, "선택한 텍스트를 자동 번역")
				if *hotkeyEnabled {
					label := comboToAccelerator(a.hotkeyCombo())
					if a.HotkeyRecording {
						label = "단축키를 눌러 주세요 · Esc 취소"
					}
					button := ui.Button(c, label).HandleInput(a.recordHotkeyInput)
					if a.HotkeyRecording {
						button.AutoFocus()
					}
					if button.Clicked() {
						if a.HotkeyRecording {
							a.cancelHotkeyRecording()
						} else {
							a.startHotkeyRecording()
						}
					}
				} else if a.HotkeyRecording {
					a.cancelHotkeyRecording()
				}
			})
		})
		ui.Row(c).Padding(actionInset).Gap(8).AlignItems(ui.Center).Children(func() {
			ui.Box(c).Grow(1)
			if ui.PrimaryButton(c, "설정 저장").Clicked() {
				if a.HotkeyRecording {
					a.HotkeyRecording = false
				}
				a.applyAppearance(a.AppearanceSel)
				a.Mu.Lock()
				a.settings.Prompt.Language = promptLanguageOf(a.PromptLanguageSel)
				a.Mu.Unlock()
				a.SaveSettings()
			}
		})
	})
}

// promptLanguageName maps the stored value onto the select label.
func promptLanguageName(l core.PromptLanguage) string {
	switch l {
	case core.PromptEnglish:
		return "영어"
	case core.PromptChinese:
		return "중국어"
	default:
		return "자동"
	}
}

// promptLanguageOf maps the select label back onto the stored value.
func promptLanguageOf(name string) core.PromptLanguage {
	switch name {
	case "영어":
		return core.PromptEnglish
	case "중국어":
		return core.PromptChinese
	default:
		return core.PromptAuto
	}
}

// applyAppearance maps and stores the appearance select state.
func (a *App) applyAppearance(name string) {
	a.Mu.Lock()
	switch name {
	case "라이트":
		a.settings.Appearance = core.AppearanceLight
	case "다크":
		a.settings.Appearance = core.AppearanceDark
	default:
		a.settings.Appearance = core.AppearanceSystem
	}
	a.Mu.Unlock()
}

// installedOption checks whether a catalog file is already present.
func (a *App) installedOption(option core.ModelOption) bool {
	a.Mu.Lock()
	defer a.Mu.Unlock()
	for _, local := range a.ModelFiles {
		if filepath.Base(local) == option.FileName() {
			return true
		}
	}
	return false
}

// engineStateView returns a copy of the engine state.
func (a *App) engineStateView() core.EngineState {
	a.Mu.Lock()
	defer a.Mu.Unlock()
	return a.EngineState
}

// engineFailure renders the failure detail, if any.
func (a *App) engineFailure() (string, bool) {
	a.Mu.Lock()
	defer a.Mu.Unlock()
	if a.EngineState.Kind != core.EngineFailed || strings.TrimSpace(a.EngineState.Detail) == "" {
		return "", false
	}
	return a.EngineState.Detail, true
}

// engineLogTail renders the last engine log lines.
func (a *App) engineLogTail() string {
	a.Mu.Lock()
	lines := a.EngineLinesRaw
	a.Mu.Unlock()
	if len(lines) == 0 {
		return ""
	}
	start := len(lines) - 12
	if start < 0 {
		start = 0
	}
	return strings.Join(lines[start:], "\n")
}

// selectedModelShort is the file name of the model in use.
func (a *App) selectedModelShort() string {
	a.Mu.Lock()
	model := deref(a.settings.Server.ModelPath)
	files := a.ModelFiles
	a.Mu.Unlock()
	if model != "" {
		return filepath.Base(model)
	}
	if picked := core.PickModel(files); picked != "" {
		return filepath.Base(picked)
	}
	return ""
}

// statsText returns the last translation statistics line.
func (a *App) statsText() string {
	a.Mu.Lock()
	defer a.Mu.Unlock()
	return a.OutputStats
}
