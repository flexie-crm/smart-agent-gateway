# Installing what the scripts need

Two libraries, one per script. Neither ships with its language, so the first run
on a machine that has not used this skill fails saying which one is missing.
That is expected. Install it and run the script again.

Do not work around a missing library by rewriting a script. The rewrite is
always worse than the library and it is thrown away on the next version of this
skill.

## Where this skill's files are

Every answer from running one of these scripts carries `skill_dir`: the folder
this version of the package is in, on this computer. Use that path when
installing anything.

Do not expect `$SAG_SKILL_DIR` to be set in a terminal. It is set for a SCRIPT
this skill runs and nowhere else, so a terminal command using it installs into
the wrong place, or sends somebody looking for the folder with `find`, which
takes a minute and finds it by luck.

## openpyxl, for `scripts/to_excel.py`

```
python3 -m pip install openpyxl
```

Then check which Python that was: `python3 -c "import sys; print(sys.executable)"`.
It has to be the one that runs the scripts, and on a machine with more than one
Python it is easy for it not to be. The failing script says which one it ran on.

### If that says "externally-managed-environment"

A recent Python on macOS (Homebrew) and on Debian or Ubuntu refuses to install
into the system interpreter. The message is long and ends with a hint; the short
version is that pip will not write into a Python the operating system manages.

Two ways through, in the order to try them.

Not every Python says this. An older pip (before 23.0) installs quietly into
the user's own directory and reports "Defaulting to user installation", which is
fine and needs nothing further.

**Install anyway, for this user.** Right when the machine is somebody's own
laptop, which is where this skill runs:

```
python3 -m pip install --break-system-packages openpyxl
```

The flag sounds worse than it is: it is the documented escape from PEP 668. It
installs into the same place a pip before 23.0 would have.

**Or an environment of its own,** when the machine is shared or somebody would
rather not touch the system interpreter:

```
python3 -m venv "<skill_dir>/.venv"
"<skill_dir>/.venv/bin/pip" install openpyxl
```

`<skill_dir>` is this version of the package. **Note what that means: a new
version of the skill is a new directory, so an environment under it has to be
made again.** Prefer the first option unless there is a reason not to.

## csv-parse, for `scripts/summary.js`

```
npm install --prefix "<skill_dir>" csv-parse
```

`<skill_dir>` is the path the script's own answer gave you. The `--prefix`
matters: it puts the library in `<skill_dir>/node_modules`,
which is where a script in this package finds it: Node looks up from the
script's own directory, so `scripts/summary.js` finds `../node_modules`. An
install into the current working directory instead puts it wherever the person
happens to be working, and it is found once and never again.

The same caveat as the venv above: a new version of the skill is a new
directory, so this is run again after an update.

## Checking it worked

Run either script again, on any CSV. If `summary.js` prints a block per column
then csv-parse is installed; if `to_excel.py` says how many rows it wrote then
openpyxl is.

## If a script still cannot find a library it has just installed

Check which interpreter ran it. `python3 -m pip install` installs into the
`python3` that ran the command, and if there are two Pythons on the machine the
one that runs the script may not be the one that got the library. `python3 -c
"import sys; print(sys.executable)"` says which one this is.
