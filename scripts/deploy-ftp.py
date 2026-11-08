#!/usr/bin/env python3
"""Deploy website/site/ to FTP server using binary mode for all files."""

import ftplib
import os
import sys

REMOTE = "/www.milkai.dev"
LOCAL = "website/site"


def upload_dir(ftp, local_dir, remote_dir):
    count = 0
    for entry in sorted(os.listdir(local_dir)):
        local_path = os.path.join(local_dir, entry)
        remote_path = f"{remote_dir}/{entry}"
        if os.path.isdir(local_path):
            try:
                ftp.mkd(remote_path)
            except ftplib.error_perm:
                pass
            count += upload_dir(ftp, local_path, remote_path)
        else:
            with open(local_path, "rb") as f:
                ftp.storbinary(f"STOR {remote_path}", f)
            count += 1
            if count % 50 == 0:
                print(f"  ...{count} files uploaded")
    return count


def main():
    host = os.environ.get("FTP_HOST", "")
    user = os.environ.get("FTP_USER", "")
    password = os.environ.get("FTP_PASS", "")
    missing = [k for k, v in (("FTP_HOST", host), ("FTP_USER", user), ("FTP_PASS", password)) if not v]
    if missing:
        print(f"Error: {', '.join(missing)} env var(s) required.", file=sys.stderr)
        sys.exit(1)

    if not os.path.isdir(LOCAL):
        print(f"Error: '{LOCAL}' not found. Run 'task site:build' first.", file=sys.stderr)
        sys.exit(1)

    print(f"Connecting to {host} as {user}...")
    ftp = ftplib.FTP(host)
    ftp.login(user, password)
    ftp.set_pasv(True)
    # Force binary mode for all transfers
    ftp.voidcmd("TYPE I")

    print(f"Uploading {LOCAL}/ → {REMOTE}")
    count = upload_dir(ftp, LOCAL, REMOTE)
    ftp.quit()
    print(f"Done. {count} files uploaded to {host}{REMOTE}")


if __name__ == "__main__":
    main()
