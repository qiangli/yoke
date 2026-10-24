package ask

import (
	"time"

	"github.com/spf13/cobra"
)

// NewRecordCmd returns `record`: ask the person in front of this computer for a
// photo, a video or a voice recording. It is ask's contract for media — a
// one-time page in their own browser, the take delivered to a private file,
// the path printed — so an agent can request a take without driving a camera
// and the person never types a command.
func NewRecordCmd() *cobra.Command {
	var o options
	cmd := &cobra.Command{
		Use:   "record photo|video|audio",
		Short: "ask the person at this computer for a photo, video or voice take (opens a camera page; prints a path)",
		Long: `record asks the person in front of this computer for a photo, a video or a
voice recording, and hands back the path of the take.

It opens a one-time page on loopback in their browser: a live preview, a
3-second countdown, Retake and "Use this". With --script the page shows the
text as a scrolling teleprompter while recording, so they only have to read.
Nothing leaves this computer; the take is written mode 0600 like an ask value.

  bashy record photo --prompt "Headshot for the founder page"
  bashy record video --script pitch.txt --max-seconds 75 --out founder.mp4

The browser asks once for camera/microphone permission. The page only opens
in a session whose screen a person can see (never a remote SSH session).`,
		Args:          cobra.ExactArgs(1),
		ValidArgs:     []string{"photo", "video", "audio"},
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(c *cobra.Command, args []string) error {
			o.media = args[0]
			o.secret = false
			return runAsk(c, &o)
		},
	}
	f := cmd.Flags()
	f.StringVar(&o.prompt, "prompt", "", "what the person sees above the camera")
	f.StringVar(&o.name, "name", "", "label for the take (letters, digits, _ . -)")
	f.StringVar(&o.script, "script", "", "video|audio: a text file shown as a scrolling teleprompter while recording")
	f.IntVar(&o.maxSecs, "max-seconds", 180, "video|audio: stop recording after this many seconds (0 = no limit)")
	f.DurationVar(&o.timeout, "timeout", 20*time.Minute, "how long to wait for the take")
	f.DurationVar(&o.ttl, "ttl", 24*time.Hour, "how long the take survives in the private store (use --out to keep it)")
	f.StringVar(&o.out, "out", "", "write the take to this path instead (created 0600, never overwritten)")
	f.BoolVar(&o.json, "json", false, "emit the result as a JSON envelope")
	return cmd
}
