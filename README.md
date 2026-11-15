# HUELLA

A macro recorder for Windows that records WHAT you clicked as well as WHERE.

One .exe. No install, no dependencies, no account, nothing leaves the machine.

A normal macro recorder stores coordinates. Move the window and it clicks on
nothing. HUELLA reads the screen while you record, so each click is kept as the
thing it landed on. On playback it finds that thing on the screen as it is now,
and clicks it there.

    1  ON    "Save"                        a button, by its words
    2  ON    box left of "Remember me"     a tick box, by what it stands beside
    3  ON    "see more details"            three words out of a long line
    4  NEAR  "Save" +39, +0                a near miss, tied to the nearest print
    5  AT    1500, 780                     nothing recognised: the point itself

Status: r1, the first working rev. It records and replays real clicks on a real
desktop. It is rough.

## Run it

1. Download `huella-r1.exe` and run it. Windows may warn about an unsigned
   program the first time. A browser tab opens at `http://127.0.0.1:9191`.
   Leave that tab open: it is the reader. It can sit in the background.
2. Press **F9**. Click through the task in any window. Press **F9** again.
3. Press **F8** to play it back. **Esc** stops a run.

The same controls are on the page. PLAY pressed there starts after 3 seconds, so
there is time to put the right window in front.

It clicks for real. Watch the first run of anything you record.

## What a run does

- It looks for each WHAT on the screen as it is now and clicks it there,
  wherever its window has moved to.
- It waits up to 8 seconds for a thing to show up, so a slow page is not a
  broken macro.
- If the thing never shows up, it stops there. It does not click the old
  coordinates and hope.
- A step recorded as a bare point (AT) is clicked where it was.

## How it works

    huella-r1.exe                              the browser tab
    -------------                              ---------------
    camera  looks about 10 times a    frame -> LINCE reads it: print, buttons,
            second; a screen that has          fields, icons
            not changed is not a frame
    ears    mouse buttons, F8 F9 Esc  click -> "was that a zone I recognise?"
    hands   SendInput              <- click    "it stands at 748,454 now"

The reader asks for the next frame the moment it has finished the last one, so
it goes as fast as the screen can be read and no faster. Most of what it reads
is thrown away. A reading matters only when a click lands: the click is answered
from the last frame taken before it, which is what was on the screen when the
hand decided.

The reader is LINCE (`cadena-lince-r3.html`, included as is): one HTML file that
turns a screenshot into click zones. It is offline and deterministic. No model
and no network at run time. The program listens on 127.0.0.1 only, and its hands
move only while a run is on.

## The macro file

Every recording is saved as `huella-macro.json` beside the program and loaded
again at the next start. A step looks like this:

    {
     "id": 1, "gap": 908, "x": 208, "y": 214, "button": "left", "count": 1,
     "screen": [1920, 1080], "win": "Notes", "read": true,
     "what": { "kind": "on", "type": "button", "text": "Save", "whole": true,
               "dx": 0, "dy": 0, "box": [173, 197, 242, 230], "nth": 0, "of": 1 }
    }

`x`, `y` is WHERE. `what` is WHAT: the words to find again, and `dx`, `dy` from
the middle of their box to the click. `nth` and `of` say which one, when the
same words stand in several places. `gap` is the pause before the step, in ms.

It is plain text on purpose. A person can check what a macro will do before it
runs, and a language model can read one as a set of instructions.

## Limits in r1

- Clicks only: left, right, double. No typing, drags or scroll yet.
- The primary screen only.
- F8, F9 and Esc are watched, not swallowed: the window in front sees them too.
- Stop with F9, or keep the HUELLA page in a browser window of its own. A click
  on its tab in a window shared with your work is recorded like any other.
- A big screen is slow to read: about a third of a second for a changed frame at
  1920x1080, about 1.4 seconds at 3840x2160.
- Small print (capitals under about 7 px tall), print over photographs and print
  cut off by a window edge are left alone rather than guessed.
- Tested with Chrome as the browser.
