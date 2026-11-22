# Pendrive Rescue 3

Free data recovery for pendrives, SD/memory cards and USB drives, for **Windows 10/11** and **Linux**.

It **only reads** the pendrive. It never writes to it, so trying it cannot make things worse.

## Download (no installation, no coding)

| Your computer | Download |
|---|---|
| **Windows 10 / 11** (almost everyone) | **[Download PendriveRescue.exe](https://github.com/Addy199904/Pendrive-Rescue/releases/latest/download/PendriveRescue.exe)** |
| Very old 32-bit Windows | [PendriveRescue-32bit.exe](https://github.com/Addy199904/Pendrive-Rescue/releases/latest/download/PendriveRescue-32bit.exe) |
| Linux | [pendrive-rescue-linux](https://github.com/Addy199904/Pendrive-Rescue/releases/latest/download/pendrive-rescue-linux) |

All versions are on the [Releases page](https://github.com/Addy199904/Pendrive-Rescue/releases/latest).

**Three steps:** download the file, double-click it, press **Check this pendrive**. That's all.
If Windows shows *"Windows protected your PC"*, click **More info** → **Run anyway** (the program is new and not yet signed, which makes Windows cautious; the source code is all in this repository).

---

## Before you start (important)

- **Stop using the pendrive.** Don't copy anything onto it, don't format it, don't run "Scan and fix" / `chkdsk`.
- If Windows says **"You need to format the disk"**, click **Cancel**.
- You need free space on your computer: at least the size of the files you want back, ideally twice the size of the pendrive.

## Windows

1. Plug in the pendrive.
2. Double-click **`PendriveRescue.exe`** (`PendriveRescue-32bit.exe` only on old 32-bit Windows).
   - If you see *"Windows protected your PC"*: click **More info**, then **Run anyway**.
   - Click **Yes** when Windows asks for administrator permission. Only administrator programs may read a pendrive directly.
3. Pendrive Rescue opens **in your web browser**. It runs only on your computer: nothing goes to the internet.
   A black window stays open next to it. Close that window (or press **Quit** in the browser) to end the program.
4. Choose the pendrive and press **Check this pendrive**. In about a minute you get a plain answer: how the pendrive is, what that means, and what Pendrive Rescue will do about it.
5. Press the button it offers. It has already picked the safest way for this pendrive (see below); other ways are listed under the button if you want one.
6. Files appear while the pendrive is read, photos grouped by month. Each one is marked **Perfect**, **Partly damaged**, **Preview only** or **Damaged**, so you know what you are getting before you save it.
7. Tick what you want and press **Save** (or save everything). Then read **REPORT.html** in the results folder.

## Linux

```
chmod +x pendrive-rescue-linux
sudo ./pendrive-rescue-linux
```

It opens in your browser the same way. Recovered files are given back to your normal user account.
`--cli` gives the step-by-step console version instead (it runs the same check first), and `--help` lists all options.

---

## The one-minute check

Before anything else, Pendrive Rescue reads several hundred places spread over the whole pendrive, each one three times, measures its speed, looks for a fake capacity and finds the file system. It never writes. Then it says what it found and decides how to read the pendrive:

| What the check says | What Pendrive Rescue does |
|---|---|
| *Your pendrive reads normally.* | Finds your files straight away. |
| *Your pendrive is slow, but it reads correctly.* | Finds your files straight away (a copy would only take longer). |
| *Your pendrive is wearing out.* The same place gave different data when read again. | **Copies it carefully** first (see below), then works from the copy. |
| *Your pendrive has parts that cannot be read.* | Copies everything readable first, so the pendrive is read as few times as possible, and tries the bad parts again at the end. |
| *This pendrive is fake.* It claims more space than it has. | Reads only the part that really exists. |
| *Nothing on your pendrive can be read*, or *it stopped answering*. | Explains that the chip that runs the pendrive has failed, which no program can get past, and gives you a note to take to a repair shop. |

## Careful copy (for pendrives that are wearing out)

Worn flash memory sometimes hands back a few wrong bits without reporting any error, and one wrong bit is enough to turn the rest of a photo grey. Reading the same place again usually gives the right data.

So a careful copy reads every part of the pendrive twice. Wherever the two reads differ, it reads that place again (at least 11 times, up to 61 times when the answers stay close) and keeps, for every single bit, what most of the reads agree on. The report says how many places were settled this way, and how many stayed unsure even after many tries. Either way: copy everything off that pendrive and stop using it.

## What the marks on each file mean

| Mark | Meaning |
|---|---|
| **Perfect** | Checked from start to end: it is complete. |
| **Partly damaged** | The top part of the photo is certainly intact (the app shows how much). Below that it may be grey or have wrong colours. The intact part is also saved as its own photo. |
| **Preview only** | The picture itself is damaged, but the small preview that cameras and phones store inside every photo is intact. It is saved next to the photo. |
| **Damaged** | Part of it is missing or wrong. It is still saved, in "4 - Possibly damaged files"; it often still opens partly. |
| **Cannot be checked** | This kind of file cannot be tested. It is saved anyway. |
| **Not checked yet** | Files that were never deleted are checked when they are saved, to spare the pendrive. |

## Photos that turn grey partway

A photo that is fine at the top and **grey (or garbled) from some point on** has wrong data from that point. Pendrive Rescue finds the exact place and fixes the usual causes:

- **Wrong bits from worn flash.** Every saved file is checked; when one fails, its damaged parts are read again **from the pendrive** and voted on bit by bit, as in the careful copy.
- **A damaged photo header.** The start of every photo holds the tables needed to decode it, and a photo whose header is damaged does not open at all. Photos from the same phone or camera share these tables, so Pendrive Rescue takes them from another photo of the same phone, but only when they match the damaged ones in almost every bit. It never invents picture content.
- **The photo was stored in pieces** and the file system no longer says where they are (after a format, or for photos found by their content). Pendrive Rescue searches the drive for the missing piece, checks that it really continues the photo, and joins it on.
- **The rest really is gone** (wiped or overwritten). Then the intact part is saved as a smaller photo, `… (intact part).jpg`, plus the photo's own small preview, `… (small preview).jpg`.

**Photos you already copied** (with Windows or another program) can be checked too: on the first screen choose **Repair photos I already copied**, or run `PendriveRescue.exe --repair "C:\path\to\photos"`. It lists every damaged photo and where it breaks, rebuilds damaged headers, combines several copies of the same photo when you have them (e.g. `IMG_1234 - Copy.jpg`), and otherwise saves the intact part and the preview. The results go into a folder called *Repaired by Pendrive Rescue* next to the photos. Copies made from a worn pendrive are best fixed by running Pendrive Rescue on the pendrive itself, which can read each damaged spot again.

## A note for a repair shop

If you still want to take the pendrive to a shop or a lab, Pendrive Rescue writes **For a repair shop.html** into the results folder. The app also offers it right after the check, so you have it even when nothing could be read. It says what the check measured, what has already been done, what a shop could still do that a program cannot, and the questions worth asking before you pay. A good shop can tell from it straight away whether they can do more.

## What you get

| Folder / file | What is inside |
|---|---|
| `1 - Files with their names` | Existing, **hidden** and **deleted** files, with their original names and folders |
| `2 - Lost folders (after format)` | Folders that a format cut off. Files and sub-folders inside keep their real names. Top-level folders are called e.g. *"Lost folder 1 - 14 photos (Mar 2024)"* |
| `3 - Found by content (deep scan)` | Files found by their content when nothing else was left. Named by type and date, e.g. *"Photo 2023-12-25 10.15.00.jpg"*, sorted into folders by type and month |
| `4 - Possibly damaged files` | Files that failed the check even after reading again. Many still open partly; photos come with their intact part and preview |
| `REPORT.html` / `REPORT.txt` | A plain-language explanation of what happened to the pendrive |
| `For a repair shop.html` | The note described above |
| `File list.csv` | Every file found, with its condition (open it with Excel) |
| `pendrive-image.img` | The copy of the pendrive, if one was made. Open it later with **Look inside this copy**. |

Every saved file is **checked**: photos are followed to their last block, iPhone photos (HEIC) and videos by their structure, ZIP/Office files by their checksums, PDFs and music by their structure. Exact duplicates are saved once.

## What it can recover

- Deleted files, including Shift+Delete and whole deleted folders
- **Quick-formatted** pendrives (FAT12/16/32, exFAT, NTFS), even if they were formatted to a *different* file system
- "RAW" pendrives, "needs to be formatted" errors, damaged boot sectors and partition tables
- Files hidden by the **shortcut virus**
- Photos with wrong bits from worn flash memory, damaged photo headers, and photos stored in pieces (see above)
- More than 50 file types by their content: phone photos and videos (JPEG, HEIC, MP4/MOV), PNG, camera RAW, PSD, MKV, AVI, 3GP, music, Office and PDF documents, archives, plain text files and notes
- Pendrives with unreadable (bad) areas: everything readable is saved, unreadable parts are tried again, and damaged files are marked
- **Fake-capacity** pendrives (a "64 GB" stick that really holds 8 GB): detected, explained, and only the real part is read

## What no software can recover (the report tells you if this is your case)

- **Full format**, "clean", or a pendrive that wiped itself (TRIM). The data is replaced by zeros or erased flash.
- Files that were **overwritten** by newer files.
- A pendrive that shows **0 bytes / no media**, or isn't detected at all: its controller chip has failed. Only a data recovery lab can help (chip-off recovery).
- BitLocker-encrypted drives without the password or recovery key.

## Good to know

- If a copy stops (for example because the pendrive disconnected), start again with the same folder: it continues where it stopped.
- If the pendrive stops answering, unplug it, plug it into another USB port (a port at the back of a desktop PC is best) and start again.
- On Windows the program needs administrator rights to read drives. It never writes to them; it also asks Windows to keep its hands off the pendrive while reading.

## Why you can trust it

- **It is open source.** Everything it does is in this folder. The programs are built the same way every time (`scripts/build.sh`): built again with the same Go version (these were built with Go 1.24.7), they come out byte for byte the same, so anyone can check that a download matches the source against the fingerprints in `SHA256SUMS.txt`.
- **It works offline.** It never connects to the internet. The browser window talks only to the program on your own computer (address 127.0.0.1).
- **Read-only is tested, not just promised.** A test reads the program's own source code and fails if anything could open a drive for writing. On Linux, a test on a real (loop) drive checks that writing is refused and that every read really reaches the drive. On Windows, the automatic tests (`.github/workflows/ci.yml`, run by GitHub on every change) make a small virtual pendrive, check that Windows cannot write to it while Pendrive Rescue holds it, then delete files from it and check that all of them come back byte for byte.
- **It is tested against failing pendrives.** `internal/sim` imitates a failing pendrive: weak bits that change between reads, unreadable areas, stalls, a fake capacity, and unplugging in the middle. The careful copy and the check are tested against it.

## Signing the Windows programs

Without a signature Windows shows *"Windows protected your PC"* before the program starts. A signature needs a certificate that says who publishes the program, so it cannot be added for you. There are two ways to get one:

1. **Free, for open-source projects: SignPath Foundation** (signpath.org). Their conditions include an OSI-approved open-source licence (add a `LICENSE` file, for example MIT or GPL-3.0), the code public on GitHub, the programs built by GitHub Actions, two-factor sign-in on both GitHub and SignPath, a version already released unsigned, and a short "Code signing policy" on the project page (see below); check their current conditions at signpath.org/terms before applying. The certificate names SignPath Foundation as the publisher. Once accepted:
   - In SignPath, create a project with the slug `pendrive-rescue`, a signing policy `release-signing`, and this artifact configuration:
     ```xml
     <artifact-configuration xmlns="http://signpath.io/artifact-configuration/v1">
       <zip-file>
         <pe-file path="PendriveRescue.exe"><authenticode-sign/></pe-file>
         <pe-file path="PendriveRescue-32bit.exe"><authenticode-sign/></pe-file>
       </zip-file>
     </artifact-configuration>
     ```
   - In the GitHub repository settings, add the variable `SIGNPATH_ORGANIZATION_ID` and the secret `SIGNPATH_API_TOKEN`.
   - From then on the `sign` job in `.github/workflows/ci.yml` sends every build to SignPath. You approve each release there, and the signed programs come back as *PendriveRescue-signed*.
   - The code signing policy they ask for can be as short as this (put your GitHub name in):

     > **Code signing policy.** Free code signing provided by SignPath.io, certificate by SignPath Foundation. Committers, reviewers and approvers: *your GitHub name*. Privacy: Pendrive Rescue sends no data over the network.
2. **Paid: a code-signing certificate from a certificate authority.** Since June 2023 its key must be kept on a hardware token or in the authority's cloud signing service, so you then sign with that authority's tool instead of the `sign` job. (Microsoft's Artifact Signing service, US$9.99 a month, takes individual developers only from the USA and Canada.)

## For developers

Written in Go (standard library only), no installation needed.

```
./scripts/build.sh                                  # all three programs into dist/, with SHA256SUMS.txt
go build -o pendrive-rescue .                       # this computer only
go run ./tools/winres -version 3.0.0 -out .         # Windows manifest, icon and version info
go test ./...                                       # tests (some need root on Linux; they skip without it)
go build -tags demosim -o pdr-demo .                # demo build: PDR_DEMO_IMAGE=disk.img adds simulated pendrives
```

`internal/disk` reads drives (Windows/Linux), `internal/health` is the one-minute check, `internal/scan` reads the whole drive once
(and does the careful copy), `internal/fsys` understands FAT/exFAT/NTFS, `internal/carve` finds files by content, `internal/verify` checks
files, `internal/jpegfix` follows JPEG data to find, cut and rebuild damaged photos and their headers, `internal/engine` runs a recovery
and writes the reports, `internal/webui` is the browser window, `internal/repair` repairs photos already copied, and `internal/sim`
imitates failing pendrives for the tests.

Pendrive Rescue is free software under the [MIT licence](LICENSE).

The app's typefaces are DM Sans and Source Serif 4 (SIL Open Font License, see `internal/webui/static/fonts/OFL.txt`).
