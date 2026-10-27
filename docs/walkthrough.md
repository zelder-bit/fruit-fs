# fruit-fs, explained

This is a plain-English tour of what fruit-fs is, how it works, and what each commit added. No prior server experience needed.

## The big picture

fruit-fs is a website that runs on a computer at home (a "home server") and lets you:

1. **Browse files** stored on that server: open folders, upload, download, rename, move, delete, and preview photos, videos, music, PDFs and text.
2. **Drop files between devices**: open fruit-fs on your phone and your laptop at the same time, and they can see each other and send files or notes back and forth, like AirDrop but in a web browser.

It is decorated with fruit: oranges and bananas are the "brand", and every kind of file gets its own fruit icon (photos are strawberries, videos are watermelon slices, music is grapes, and so on).

### The pieces

- **The server program** is written in **Go**, a programming language that compiles into a single file you can run. That one file contains the whole app, including the web pages, so there is nothing else to install.
- **The web pages** are plain HTML, CSS (styling) and JavaScript (behaviour). They are tucked inside the Go program when it is built.
- **The container**: the program is packaged as a **Docker image**. Think of a container as a lunchbox: it holds the app and exactly what it needs, so it runs the same way on any computer. The home server runs that lunchbox inside a small virtual computer (an "LXC container") on Proxmox, the software that manages the home server's virtual machines.
- **The files** live on the server's big 1 TB hard drive. The app can only see one folder on it, called the *data folder*.

### How you get in

There are two doors, and the app checks which one you came through:

- **Tailscale door.** Tailscale is a private network that connects your own devices over the internet, like a secret club only your devices belong to. When you visit fruit-fs through Tailscale, Tailscale itself tells the app who you are (your Tailscale login), so you don't need a password. The app only believes that "who you are" note if it came from the home server's own address, because that is where Tailscale hands requests over. Anyone else trying to write that note gets ignored.
- **Home Wi-Fi door.** If you type the server's local address while on your home network, there is no Tailscale note, so the app asks for a password. The password itself is never stored, only a scrambled "fingerprint" of it (an *argon2id hash*), so even someone who reads the settings can't learn the password. After you log in, your browser gets a *cookie* (a signed ticket) so you stay logged in for 30 days.

### Keeping it safe

- **No escaping the data folder.** Every file action goes through Go's `os.Root`, which works like a fence: asking for `../../secret-stuff` or following a shortcut (symlink) that points outside simply fails.
- **No sneaky websites.** Another website you visit can't make your browser secretly delete your files, because every change must include a special header (`X-Fruit: 1`) that other sites aren't allowed to add.
- **No running uploaded web pages.** If someone uploads an `.html` file, fruit-fs only lets you download it, never open it as a live page on the site, so it can't run code.
- **Nothing private in this repo.** This code is public on GitHub, so real addresses, names and passwords live only in a settings file (`.env`) on the server. A checker script runs before every commit to catch mistakes.

## Commit by commit

Each section below matches one commit, oldest first.

### 1. Groundwork and the leak checker

- Set up the project folders, the MIT license (anyone may use the code), and `.gitignore`, a list of files git must never save, such as `.env` with the real settings.
- Added `scripts/check-public.sh`. Before each commit it scans the new lines for things that look private: home network addresses (like `192.168.x.x`), Tailscale machine names, email addresses, network card IDs (MAC addresses), private keys and home-folder paths. If it finds any, the commit is stopped. It also asks a small AI model running on this computer (through Ollama, so nothing leaves the machine) for a second opinion.
- Set this repo's git author email to GitHub's private "noreply" address, so the personal email never appears in the public history.

### 2. The server: logins, files, uploads and the drop

This commit is the "back end", the part that runs on the server and that the web pages talk to. The pages ask for things with short web requests called an **API** (for example `GET /api/ls?path=photos` means "list the photos folder"), and the server answers with **JSON**, a simple text format for data.

The code is split into small packages, each with one job:

- **`internal/config`** reads the settings from environment variables (named settings handed to the program when it starts, like `FRUITFS_DATA=/data`). If nobody would be able to log in, it refuses to start rather than run wide open.
- **`internal/auth`** is the bouncer described above: it trusts the Tailscale "who you are" note only from the home server's address, handles the password login, and hands out signed cookies. A signed cookie is like a ticket with a tamper-proof stamp: the server can tell if anyone edited it, and it expires after 30 days. After 10 wrong passwords from one device, that device has to wait 15 minutes.
- **`internal/files`** is the file browser: list a folder, download, preview, make folders, rename/move, and search by name. Whole folders download as a **zip** that is built *while* it downloads (streaming), so even a huge folder doesn't need to be packed up first. Deleting doesn't destroy anything straight away: items go to a hidden **trash** and are only wiped after 30 days, so mistakes can be undone.
- **`internal/upload`** receives files in **chunks** (pieces of 8 MB). If the Wi-Fi drops halfway through a 4 GB video, the browser asks "how much did you get?" and continues from there instead of starting over. A file only appears in its folder once the last chunk arrives, so you never see half a file.
- **`internal/drop`** runs the device-to-device drop. Every open tab connects with a **WebSocket**, a phone line that stays open so the server can push messages instantly. Each tab gets a fun name like "Zesty Orange". When one tab offers a file and the other accepts, the sender uploads and the receiver downloads *at the same time*, and the server joins the two like a pipe, so the file is never stored on the server. The receiver can also choose "save to fruit-fs", which puts the file in a `Drop inbox` folder instead.
- **`cmd/fruit-fs`** is the starting point that wires everything together. It can also make a password fingerprint (`fruit-fs hash-password`) and check that the server is healthy (`fruit-fs healthcheck`), which Docker uses to restart the app if it ever stops answering.

**Tests.** Each package has automatic tests, small programs that try things and check the answers. They try to break in on purpose: faking the Tailscale note from the wrong address, sneaking out of the data folder with `../` or a shortcut, uploading over an existing file, cutting an upload off halfway and resuming it, another user grabbing someone's drop, and a different website trying to open the drop connection. All of them must fail safely for the tests to pass. They run inside Docker with `scripts/go.sh test ./...`, so Go doesn't even need to be installed.

### 3. Fixes from a second reviewer

A different AI coding tool (OpenAI's Codex) read the server code with fresh eyes and acted like an attacker, the way a school might bring in an outside inspector. It found 11 problems. None could be used by a stranger on the internet, but they were worth fixing:

- **The cookie key moved out of the shared folder.** The secret key that stamps login cookies used to live in a hidden folder inside the data folder. If someone ever made a shortcut (symlink) pointing at that hidden folder, a signed-in user could have read the key and forged cookies. The key now lives in a separate `state` folder that the file browser can't reach at all (`FRUITFS_STATE`). Changing the password now also signs everyone out.
- **Password guessing can't be done in bulk.** Before, a script could fire off many guesses at the same instant, before any of them counted as a failure. Each guess is now counted the moment it arrives, and only two password checks can run at once. Each check deliberately uses 64 MB of memory to make guessing slow, so letting hundreds run together could have overloaded the server.
- **Stalled connections get hung up.** A request that sends its data very slowly, or stops halfway, now gets cut off: a login has 15 seconds, and a transfer may run for hours as long as data keeps moving, but two minutes of silence ends it.
- **Limits on everything.** There are now caps on unfinished uploads, connected devices and pending drops, and "save to fruit-fs" follows the same size limit as normal uploads.
- **Drop cleans up after itself.** If either device disconnects mid-transfer, the other side is told and the server lets go of the connection. Each file in a drop can be claimed by only one sender and one receiver. And "Delivered" is only shown once a saved file is really on the disk.
- **No accidental overwrites.** Two uploads with the same name finishing at the same moment used to be able to overwrite each other. Finishing now uses a *hard link*, a second name for the same file that the operating system refuses to create if the name is already taken, so exactly one upload wins and the other gets a "name taken" message.
- **Odd files can't freeze it.** A *FIFO* is a special file that waits forever for someone to write into it. Opening one used to hang the request; now the server checks what kind of file it is first.
- **Smaller fixes:** signing out also requires the app's own header, and turning off password login now also cancels existing password sessions.

New tests check each of these, for example 30 password guesses at once (at least 20 must be refused), cancelling an upload mid-way, and a receiver hanging up partway through a 64 MB drop.

### 4. The web pages and the fruit look

This commit is the "front end", what you actually see and click. It is written in plain JavaScript with no build tools or downloaded libraries: the browser runs the files exactly as written. That keeps the project small, and there is no pile of other people's code that could hide a problem.

**How the pages are organised** (`web/static/`):

- `index.html` is the skeleton: the sign-in card, the top bar with the Files / Drop / Trash tabs, and empty boxes the JavaScript fills in.
- `js/app.js` starts everything. It asks the server "who am I?" and shows either the sign-in card or the app. It also switches tabs based on the part of the address after `#`, so the browser's Back button works and each folder has its own link.
- `js/files.js` draws the file browser: cards in a grid (or rows in a list), the breadcrumb trail ("Home › Photos"), search, the ⋯ menu (preview, download, rename, move, delete), ticking several files to zip, move or delete them together, and the preview window (photos, videos, music, PDFs and text, with ‹ › arrows to flip through).
- `js/upload.js` is the upload queue. It cuts each file into 8 MB pieces, sends two files at a time, shows progress, and if the connection drops it asks the server how much arrived and picks up from there. Dragging a whole folder onto the page keeps its sub-folders.
- `js/drop.js` runs the Drop tab: it keeps a live connection open, shows the other devices as bubbles, lets you tap one to send files or long-press it to send a note, and pops up "Zesty Orange wants to share" with **Download**, **Save to fruit-fs** or **Decline**.
- `js/ui.js` holds small shared helpers. The most important is `h()`, which builds every bit of the page. It always treats names as plain text, never as code, so a file named `<script>…` just shows up as those characters instead of running.

**The fruit look** (`fruit.css` and `svg/`):

- Colours are defined once as named variables (orange `#FF8A00`, banana `#FFE135`, leaf green, cream background) and reused everywhere. Dark mode, called "night grove", swaps those variables for a darker orchard palette. It follows your device's setting, and the ☾ button overrides it.
- Every kind of file has a fruit: folders are fruit crates, photos strawberries, videos watermelon, music grapes, documents lemons, PDFs peaches, zips coconuts, code kiwis, and anything else an apple. Photos show a small preview instead.
- Small touches: the upload bar is a banana sliding along, the drop target is a spinning orange slice, the trash is a "compost bin", and empty folders show an empty crate with a banana peel.
- The fruit drawings were first drafted by Google's Gemini model, then checked by hand: no scripts, no outside links, and colours tied to the variables above.
- The fonts (Fredoka for headings, Nunito for text) are stored inside the app instead of loaded from Google, so the page works with no internet and nobody else learns when you open it. Their open-source licences are included.
- On phones the tabs move under the header, the buttons share one row, and previews fill the screen.

**How it was checked:** a headless browser (a real Chrome with no window, driven by a script) signed in, opened folders, previewed a photo, uploaded a 20 MB file, deleted it and found it in Trash, switched to dark mode, sent a file between two tabs through Drop, and loaded the page at phone width to make sure nothing spilled off the side of the screen.

### 5. Packing the lunchbox (the container)

This commit packages fruit-fs so the home server can run it with one command.

- **`Dockerfile`** is the recipe for the container image, in two stages. Stage one uses a big image that has Go installed and compiles the program. Stage two starts from *distroless*, a nearly empty base image with no shell and no extra programs, and copies in just the finished program. The result is about 10 MB, and an attacker who somehow got inside would find almost no tools to work with. The app runs as an ordinary user (number 65532), not as the all-powerful "root" user.
- **Health check.** Every 30 seconds Docker runs `fruit-fs healthcheck`, which asks the server "are you OK?". If it stops answering, Docker marks it unhealthy and can restart it.
- **`compose.yaml`** is a settings file for Docker Compose, the tool that starts the container. It runs the app with a read-only system disk, drops every special permission, and connects two folders from the server: `data` (your files) and `state` (the cookie key). It uses *host networking*, meaning the app sees the real address of whoever connects. That matters because the Tailscale check depends on knowing exactly where a request came from.
- **`.env.example`** is a fill-in-the-blanks copy of the settings. You copy it to `.env`, which never gets committed. A gotcha found while testing: the password fingerprint contains `$` signs, and Compose thinks anything starting with `$` is a variable to fill in, which garbled the hash. Wrapping it in single quotes stops that, and the server now also strips stray quotes in case a different tool passes them through.
- **GitHub Actions** (`.github/workflows/image.yml`) is a robot on GitHub's computers. Every time code is pushed, it runs all the tests. When a version tag like `v0.1.0` is pushed, it also builds the image and publishes it to GitHub's container registry, so the home server can simply download it.
- Uploaded files are now saved readable by other users on the server, not just by the app, so they can be reached from the host computer too.
- `README.md` explains how to set it up, every setting, and the security choices.

Tested by running the real image with Compose: it reported healthy, accepted the password, saved an upload, and wrote files owned by the app's user.
