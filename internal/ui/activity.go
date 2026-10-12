package ui

import (
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/lucasb-eyer/go-colorful"
)

const (
	activityWidth       = 15
	activityInterval    = 50 * time.Millisecond
	activityBirthFrames = 20
	activityGlyphs      = "0123456789abcdefABCDEF~!@#$%^&*()+=_"
)

// Each tick belongs to one run. An old timer cannot start a second animation
// loop if another task begins before that timer has delivered its message.
type activityTickMsg struct {
	generation uint64
	frame      uint64
}

type activityModel struct {
	generation uint64
	frame      uint64
	active     bool
}

func (a *activityModel) start() tea.Cmd {
	a.generation++
	a.frame = 0
	a.active = true
	return a.tick()
}

func (a *activityModel) stop() { a.active = false }

func (a activityModel) tick() tea.Cmd {
	msg := activityTickMsg{generation: a.generation, frame: a.frame + 1}
	return tea.Tick(activityInterval, func(time.Time) tea.Msg { return msg })
}

func (a *activityModel) update(msg activityTickMsg) tea.Cmd {
	if !a.active || msg.generation != a.generation || msg.frame != a.frame+1 {
		return nil
	}
	a.frame = msg.frame
	return a.tick()
}

// View is pure: redraws caused by streaming text or typing keep the same frame.
// Each cell scrambles independently while the color wave travels across it.
// ASCII keeps the footprint fixed across terminal fonts.
func (a activityModel) View() string {
	var out strings.Builder
	seed := activityHash(a.generation + 0x9e3779b97f4a7c15)
	for i := range activityWidth {
		birth := 1 + activityHash(seed^uint64(i+1)*0x9e3779b97f4a7c15)%(activityBirthFrames-1)
		glyph := byte('.')
		style := activityGradient[(a.frame+uint64(i))%uint64(len(activityGradient))]
		if a.frame < birth {
			style = style.Faint(true)
		} else {
			// Give every frame its own set of cells. Using frame+i here
			// makes the previous frame slide left instead of scrambling.
			x := activityHash(seed ^ (a.frame*activityWidth + uint64(i)))
			glyph = activityGlyphs[x%uint64(len(activityGlyphs))]
		}
		out.WriteString(style.Render(string(glyph)))
	}
	return out.String()
}

func activityHash(x uint64) uint64 {
	x ^= x >> 30
	x *= 0xbf58476d1ce4e5b9
	x ^= x >> 27
	x *= 0x94d049bb133111eb
	return x ^ (x >> 31)
}

// Precompute a seamless violet-to-coral wave, the interface's two accents, in
// perceptual color space.
var activityGradient = func() [activityWidth * 2]lipgloss.Style {
	purple := colorful.Color{R: 0xC6 / 255.0, G: 0xB6 / 255.0, B: 0xF2 / 255.0}
	magenta := colorful.Color{R: 0xF4 / 255.0, G: 0x9D / 255.0, B: 0x89 / 255.0}
	var gradient [activityWidth * 2]lipgloss.Style
	for i := range gradient {
		position := i
		if position > activityWidth {
			position = len(gradient) - position
		}
		color := purple.BlendHcl(magenta, float64(position)/activityWidth).Clamped()
		gradient[i] = lipgloss.NewStyle().Foreground(color)
	}
	return gradient
}()
