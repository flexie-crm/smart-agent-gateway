; What the installer does that Tauri's template does not: empty the directory
; before filling it.
;
; Wired in by tauri.windows.conf.json (bundle.windows.nsis.installerHooks), and
; inserted at the top of Section Install, before the first File.
;
; WHY. The template writes the new build's files one by one over whatever is
; there and removes nothing, and its uninstaller deletes an enumerated list
; generated from the build that wrote it. So every upgrade leaves the previous
; build's content-hashed assets behind for ever, and only the last generation is
; ever cleaned up. Measured on this machine after five installs:
; console\assets held five index-*.js and three index-*.css, one per install.
; Worse than clutter, an upgrade that stops shipping a component leaves that
; component on the disk and runnable: the build that removed the inference engine
; left 117.9 MB of sag-inference-cpu.exe behind, inert only because nothing looks
; for it any more.
;
; And it is not an edge case. The template asks on a GUI upgrade whether to
; uninstall first and takes no for an answer (installer.nsi, PageLeaveReinstall),
; and under /UPDATE, which is what the in-app updater passes, it does not ask at
; all: "In update mode, always proceeds without uninstalling".
;
; WHOLESALE, rather than a list of the directories we know about. A list is a
; thing that drifts: it would have to have grown a line the day the engine
; arrived and lost one the day it left, and the cost of forgetting is exactly the
; defect this fixes, silently. The rule is simpler and does not rot: this
; directory is ours, and an install replaces all of it.
;
; WHAT IS NOT TOUCHED is everything a person would mind losing. Their
; conversations, keys and database are in %APPDATA%\SAG Personal and the webview's
; storage is under %LOCALAPPDATA%\io.flexie.sag.personal; neither is inside
; $INSTDIR, and somebody upgrading has not asked to lose either.

!macro NSIS_HOOK_PREINSTALL
  ; Only a directory that is already ours. uninstall.exe is written there by this
  ; installer and by nothing else, so its absence means there is no previous
  ; install here to clear and $INSTDIR may be anything at all, including a
  ; directory somebody typed on the directory page.
  ${If} ${FileExists} "$INSTDIR\uninstall.exe"
    ; Stopped before it is emptied, not after. The template runs this check
    ; itself a few lines below, which is too late: the gateway serves the console
    ; and the chat out of this directory, and taking them away underneath a
    ; running application is a working window that starts answering 404.
    ;
    ; It is the same macro the template uses, so there is one behaviour and not
    ; two: silent under /S, and a cancellable prompt otherwise. Running it twice
    ; is free, because by then there is nothing left to find.
    !insertmacro CheckIfAppIsRunning "${MAINBINARYNAME}.exe" "${PRODUCTNAME}"

    ; And the gateway, which is NOT the application and does not go with it. It
    ; is started detached, in a process group of its own, holding the database
    ; (desktop/personal/shell/src/gateway.rs), so the check above can close the
    ; window and leave sag.exe running. Windows will not overwrite a running
    ; image, so the install would then fail on it.
    ;
    ; Asked to stop the way the application asks, which is a file in the state
    ; directory it is already watching for (orchestrator/cmd/sag/personal_stop.go):
    ; a drain and a clean database shutdown rather than a kill. That directory's
    ; name is written here for the third time, after gateway.rs and the
    ; uninstaller's own $APPDATA line; if it ever changes, all three move.
    SetShellVarContext current
    ClearErrors
    FileOpen $0 "$APPDATA\SAG Personal\stop" w
    ${IfNot} ${Errors}
      FileClose $0
    ${EndIf}

    ; Thirty seconds, which is what the application itself allows before it
    ; insists (STOP_GRACE in gateway.rs). Nothing here insists: an installer is
    ; not the right thing to be force-killing a database from.
    StrCpy $R9 0
    sag_still_running:
      nsis_tauri_utils::FindProcessCurrentUser "sag.exe"
      Pop $R8
      ${If} $R8 != 0
        Goto sag_gone
      ${EndIf}
      Sleep 500
      IntOp $R9 $R9 + 1
      ${If} $R9 < 60
        Goto sag_still_running
      ${EndIf}
      ; Still there after all that. Stop BEFORE removing anything: an upgrade
      ; that fails and leaves the working version in place is a bad afternoon,
      ; and one that empties the directory first and then fails is a reinstall.
      Abort "SAG Personal is still running and would not stop. Close it and run this again."
    sag_gone:

    ; Out of the directory before removing it. Section Install has already made
    ; $INSTDIR the working directory, and Windows will not delete the working
    ; directory of a running process: RMDir would empty it, fail on the last
    ; step, and leave a success that depended on a detail nobody stated.
    SetOutPath $TEMP
    RMDir /r "$INSTDIR"
    SetOutPath $INSTDIR
  ${EndIf}
!macroend
