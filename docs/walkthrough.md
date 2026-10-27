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
