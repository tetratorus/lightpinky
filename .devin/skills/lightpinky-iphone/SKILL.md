---
name: lightpinky-iphone
description: Set up lightpinky and control a real iPhone or the iOS Simulator from a Mac, including waking, unlocking, screenshots, screen elements, touch, typing and apps.
---

# lightpinky

Use https://github.com/tetratorus/lightpinky. Clone it if needed, then follow the repository README's **Setup** instructions. If it is already cloned, update it every time before use: `git pull --ff-only`, then `python3 scripts/setup.py`. If uncommitted changes or a diverged branch block the pull, say so instead of discarding anything. The repository owns installation and troubleshooting; do not invent a separate setup.

For a real iPhone, setup is not complete until remote unlock is verified. Device discovery or an already-unlocked phone does not prove this works: run `scripts/phone unlock` with the phone locked and confirm it becomes unlocked. If `IPHONE_UNLOCK_CODE` is unavailable, explicitly ask the owner to set it locally in the server's environment or `~/.zshrc` as described in the README, then retry verification once configured. Never ask for the code in chat, print it, or commit it. If verification needs the owner's help, explicitly report setup as incomplete. Never automatically repeat a passcode attempt.

`scripts/phone tools` discovers the complete interface. `scripts/phone unlock 'launch={"app":"Shopee"}' elements screenshot` is one example; the tools work with other apps and tasks too. The README explains the locally configured `IPHONE_UNLOCK_CODE`, Wi-Fi discovery, first-time pairing and developer disk images.

A Simulator has no passcode; setup for it is complete once `scripts/phone devices` lists it and `elements` reads its screen.
