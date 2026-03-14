# Installing Hermod

Hermod is available for **Linux**, **macOS**, and **Windows** on both `amd64` (x86_64) and `arm64` (Apple Silicon / ARM) architectures.

Choose the method that suits your platform:

| Method | Linux | macOS | Windows |
|--------|-------|-------|---------|
| One-liner script | Yes | Yes | No |
| Homebrew | Yes | Yes | No |
| Manual download | Yes | Yes | Yes |

---

## Method 1: One-Liner Script (Linux & macOS)

The fastest way to install. Run this in your terminal:

```sh
curl -sSfL https://raw.githubusercontent.com/MFZNK05/DFS-Go/main/install.sh | sh
```

Or if you only have `wget`:

```sh
wget -qO- https://raw.githubusercontent.com/MFZNK05/DFS-Go/main/install.sh | sh
```

### What the script does

1. Detects your OS (`linux` or `darwin`) and architecture (`amd64` or `arm64`)
2. Fetches the latest release from GitHub
3. Downloads the `.tar.gz` archive and `checksums.txt`
4. Verifies the SHA-256 checksum
5. Extracts the `hermod` binary
6. Installs to `/usr/local/bin` (if writable) or `~/.local/bin` (fallback)

### After installation

If installed to `/usr/local/bin` — you're done, it's already in your PATH.

If installed to `~/.local/bin`, the script prints a warning. Add this to your shell config:

**Bash** (`~/.bashrc`):
```sh
export PATH="$HOME/.local/bin:$PATH"
```

**Zsh** (`~/.zshrc`):
```sh
export PATH="$HOME/.local/bin:$PATH"
```

Then reload your shell:
```sh
source ~/.bashrc   # or source ~/.zshrc
```

### Verify

```sh
hermod version
```

---

## Method 2: Homebrew (macOS & Linux)

```sh
brew install MFZNK05/hermod/hermod
```

Or in two steps:

```sh
brew tap MFZNK05/hermod
brew install hermod
```

### Upgrade

```sh
brew upgrade hermod
```

### Uninstall

```sh
brew uninstall hermod
brew untap MFZNK05/hermod
```

### Verify

```sh
hermod version
```

---

## Method 3: Manual Download (Linux, macOS, Windows)

Download the appropriate archive from the [GitHub Releases page](https://github.com/MFZNK05/DFS-Go/releases/latest).

### Available archives

| Platform | Architecture | Filename |
|----------|-------------|----------|
| Linux | x86_64 | `hermod_<version>_linux_amd64.tar.gz` |
| Linux | ARM64 | `hermod_<version>_linux_arm64.tar.gz` |
| macOS | Intel | `hermod_<version>_darwin_amd64.tar.gz` |
| macOS | Apple Silicon | `hermod_<version>_darwin_arm64.tar.gz` |
| Windows | x86_64 | `hermod_<version>_windows_amd64.zip` |
| Windows | ARM64 | `hermod_<version>_windows_arm64.zip` |

---

### Linux

```sh
# Download (replace <version> with the actual version, e.g. 0.1.3)
curl -LO https://github.com/MFZNK05/DFS-Go/releases/download/v<version>/hermod_<version>_linux_amd64.tar.gz

# Extract
tar -xzf hermod_<version>_linux_amd64.tar.gz

# Move to a directory in your PATH
sudo mv hermod /usr/local/bin/

# Or without sudo:
mkdir -p ~/.local/bin
mv hermod ~/.local/bin/
# Add ~/.local/bin to PATH if not already (see "After installation" above)

# Verify
hermod version
```

---

### macOS

**Intel Mac:**
```sh
curl -LO https://github.com/MFZNK05/DFS-Go/releases/download/v<version>/hermod_<version>_darwin_amd64.tar.gz
tar -xzf hermod_<version>_darwin_amd64.tar.gz
sudo mv hermod /usr/local/bin/
hermod version
```

**Apple Silicon (M1/M2/M3/M4):**
```sh
curl -LO https://github.com/MFZNK05/DFS-Go/releases/download/v<version>/hermod_<version>_darwin_arm64.tar.gz
tar -xzf hermod_<version>_darwin_arm64.tar.gz
sudo mv hermod /usr/local/bin/
hermod version
```

**macOS Gatekeeper note:** If macOS blocks the binary with "cannot be opened because the developer cannot be verified", run:
```sh
xattr -d com.apple.quarantine /usr/local/bin/hermod
```

---

### Windows

1. Go to the [Releases page](https://github.com/MFZNK05/DFS-Go/releases/latest)
2. Download `hermod_<version>_windows_amd64.zip` (or `arm64` for ARM devices)
3. Extract the `.zip` file (right-click > "Extract All" or use 7-Zip)
4. Move `hermod.exe` to a permanent location, e.g.:
   ```
   C:\Program Files\hermod\hermod.exe
   ```
5. Add the folder to your system PATH:
   - Press `Win + R`, type `sysdm.cpl`, press Enter
   - Go to **Advanced** tab > **Environment Variables**
   - Under **System variables**, find `Path`, click **Edit**
   - Click **New** and add: `C:\Program Files\hermod`
   - Click **OK** on all dialogs
6. Open a **new** Command Prompt or PowerShell and verify:
   ```
   hermod version
   ```

**PowerShell alternative** (no GUI needed):
```powershell
# Download
Invoke-WebRequest -Uri "https://github.com/MFZNK05/DFS-Go/releases/download/v<version>/hermod_<version>_windows_amd64.zip" -OutFile hermod.zip

# Extract
Expand-Archive hermod.zip -DestinationPath "$env:LOCALAPPDATA\hermod"

# Add to PATH (current user, persistent)
$p = [Environment]::GetEnvironmentVariable("Path", "User")
[Environment]::SetEnvironmentVariable("Path", "$p;$env:LOCALAPPDATA\hermod", "User")

# Restart your terminal, then:
hermod version
```

---

## Verifying Checksums

Every release includes a `checksums.txt` file with SHA-256 hashes for all archives. To verify manually:

**Linux:**
```sh
sha256sum hermod_<version>_linux_amd64.tar.gz
# Compare output with the hash in checksums.txt
```

**macOS:**
```sh
shasum -a 256 hermod_<version>_darwin_arm64.tar.gz
```

**Windows (PowerShell):**
```powershell
Get-FileHash hermod_<version>_windows_amd64.zip -Algorithm SHA256
```

---

## Uninstalling

**One-liner / manual install:**
```sh
rm $(which hermod)
```

**Homebrew:**
```sh
brew uninstall hermod
```

**Windows:**
Delete `hermod.exe` and remove the folder from your PATH environment variable.

---

## Troubleshooting

| Problem | Solution |
|---------|----------|
| `command not found: hermod` | The binary isn't in your PATH. Check where it was installed and add that directory to PATH. |
| macOS "developer cannot be verified" | Run `xattr -d com.apple.quarantine /usr/local/bin/hermod` |
| Permission denied on `/usr/local/bin` | Use `sudo mv` or install to `~/.local/bin` instead |
| Windows "not recognized as a command" | Open a **new** terminal after editing PATH. Verify the folder containing `hermod.exe` is in your PATH. |
| Checksum mismatch | Re-download the archive. If it persists, report it as an issue. |
