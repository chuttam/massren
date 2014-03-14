package main

import (
	"crypto/md5"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/jessevdk/go-flags"
	"github.com/kr/text"
	"github.com/laurent22/go-trash"
)

var flagParser_ *flags.Parser
var newline_ string

const (
	APPNAME     = "massren"
	LINE_LENGTH = 80
	KIND_RENAME = 1
	KIND_DELETE = 2
)

type CommandLineOptions struct {
	DryRun  bool `short:"n" long:"dry-run" description:"Don't rename anything but show the operation that would have been performed."`
	Verbose bool `short:"v" long:"verbose" description:"Enable verbose output."`
	Config  bool `short:"c" long:"config" description:"Set a configuration value. eg. massren --config <name> [value]"`
	Undo    bool `short:"u" long:"undo" description:"Undo a rename operation. Currently delete operations cannot be undone (though files can be recovered from the trash in OSX and Windows). eg. massren --undo [path]"`
	Version bool `short:"V" long:"version" description:"Displays version information."`
}

type FileAction struct {
	oldPath string
	newPath string
	kind    int
}

func NewFileAction() *FileAction {
	output := new(FileAction)
	output.kind = KIND_RENAME
	return output
}

func (this *FileAction) FullOldPath() string {
	return normalizePath(this.oldPath)
}

func (this *FileAction) FullNewPath() string {
	return normalizePath(filepath.Join(filepath.Dir(this.oldPath), filepath.Base(this.newPath)))
}

func (this *FileAction) String() string {
	return fmt.Sprintf("Kind: %d; Old: \"%s\"; New: \"%s\"", this.kind, this.oldPath, this.newPath)
}

func stringHash(s string) string {
	h := md5.New()
	io.WriteString(h, s)
	return fmt.Sprintf("%x", h.Sum(nil))
}

func tempFolder() string {
	output := filepath.Join(profileFolder(), "temp")
	err := os.MkdirAll(output, PROFILE_PERM)
	if err != nil {
		panic(err)
	}
	return output
}

func criticalError(err error) {
	logError("%s", err)
	logInfo("Run '%s --help' for usage\n", APPNAME)
	os.Exit(1)
}

func watchFile(filePath string) error {
	initialStat, err := os.Stat(filePath)
	if err != nil {
		return err
	}

	for {
		stat, err := os.Stat(filePath)
		if err != nil {
			return err
		}

		if stat.Size() != initialStat.Size() || stat.ModTime() != initialStat.ModTime() {
			return nil
		}

		time.Sleep(1 * time.Second)
	}
}

func newline() string {
	if newline_ != "" {
		return newline_
	}

	if runtime.GOOS == "windows" {
		newline_ = "\r\n"
	} else {
		newline_ = "\n"
	}

	return newline_
}

func guessEditorCommand() (string, error) {
	switch runtime.GOOS {

	case "windows":

		return "notepad.exe", nil

	default: // assumes a POSIX system

		// Get it from EDITOR environment variable, if present
		editorEnv := strings.Trim(os.Getenv("EDITOR"), "\n\t\r ")
		if editorEnv != "" {
			return editorEnv, nil
		}

		// Otherwise, try to detect various text editors
		editors := []string{
			"nano",
			"vim",
			"emacs",
			"vi",
			"ed",
		}

		for _, editor := range editors {
			err := exec.Command("type", editor).Run()
			if err == nil {
				return editor, nil
			} else {
				err = exec.Command("sh", "-c", "type "+editor).Run()
				if err == nil {
					return editor, nil
				}
			}
		}

	}

	return "", errors.New("could not guess editor command")
}

func editFile(filePath string) error {
	var err error
	editorCmd := config_.String("editor")
	if editorCmd == "" {
		editorCmd, err = guessEditorCommand()
		setupInfo := fmt.Sprintf("Run `%s --config editor \"name-of-editor\"` to set up the editor. eg. `%s --config editor \"vim\"`", APPNAME, APPNAME)
		if err != nil {
			criticalError(errors.New(fmt.Sprintf("No text editor defined in configuration, and could not guess a text editor.\n%s", setupInfo)))
		} else {
			logInfo("No text editor defined in configuration. Using \"%s\" as default.\n%s", editorCmd, setupInfo)
		}
	}

	pieces := strings.Split(editorCmd, " ")
	pieces = append(pieces, filePath)
	cmd := exec.Command(pieces[0], pieces[1:]...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	err = cmd.Run()

	if err != nil {
		return err
	}
	return nil
}

func filePathsFromArgs(args []string) ([]string, error) {
	var output []string
	var err error

	if len(args) == 0 {
		output, err = filepath.Glob("*")
		if err != nil {
			return []string{}, err
		}
	} else {
		for _, arg := range args {
			if strings.Index(arg, "*") < 0 && strings.Index(arg, "?") < 0 {
				output = append(output, arg)
				continue
			}
			matches, err := filepath.Glob(arg)
			if err != nil {
				return []string{}, err
			}
			for _, match := range matches {
				output = append(output, match)
			}
		}
	}

	sort.Strings(output)

	return output, nil
}

func stripBom(s string) string {
	if len(s) < 3 {
		return s
	}
	if s[0] != 239 || s[1] != 187 || s[2] != 191 {
		return s
	}
	return s[3:]
}

func filePathsFromString(content string) []string {
	var output []string
	lines := strings.Split(content, newline())
	for i, line := range lines {
		line := strings.Trim(line, "\n\r")
		if i == 0 {
			line = stripBom(line)
		}
		if line == "" {
			continue
		}
		if len(line) >= 2 && line[0:2] == "//" {
			continue
		}
		output = append(output, line)
	}

	return output
}

func filePathsFromListFile(filePath string) ([]string, error) {
	contentB, err := ioutil.ReadFile(filePath)
	if err != nil {
		return []string{}, err
	}

	return filePathsFromString(string(contentB)), nil
}

func printHelp() {
	flagParser_.WriteHelp(os.Stdout)

	examples := `
Examples:

  Process all the files in the current directory:
  % APPNAME	
  
  Process all the JPEGs in the specified directory:
  % APPNAME /path/to/photos/*.jpg
  
  Undo the changes done by the previous operation:
  % APPNAME --undo /path/to/photos/*.jpg

  Set VIM as the default text editor:
  % APPNAME --config editor vim
  
  List config values:
  % APPNAME --config
`
	fmt.Println(strings.Replace(examples, "APPNAME", APPNAME, -1))
}

func fileActions(originalFilePaths []string, changedContent string) ([]*FileAction, error) {
	if len(originalFilePaths) == 0 {
		return []*FileAction{}, nil
	}
	lines := strings.Split(changedContent, newline())
	fileIndex := 0

	var actionKind int
	var output []*FileAction

	for i, line := range lines {
		line := strings.Trim(line, "\n\r")

		if i == 0 {
			line = stripBom(line)
		}

		if line == "" {
			continue
		}

		oldBasePath := filepath.Base(originalFilePaths[fileIndex])
		newBasePath := ""
		if len(line) >= 2 && line[0:2] == "//" {
			// Check if it is a comment or a file being deleted.
			newBasePath = strings.Trim(line[2:], " \t")
			if newBasePath != strings.Trim(oldBasePath, " \t") {
				// This is not a file being deleted, it's
				// just a regular comment.
				continue
			}
			newBasePath = ""
			actionKind = KIND_DELETE
		} else {
			newBasePath = line
			actionKind = KIND_RENAME
		}

		if actionKind == KIND_RENAME && newBasePath == oldBasePath {
			// Found a match but nothing to actually rename
		} else {
			action := NewFileAction()
			action.kind = actionKind
			action.oldPath = originalFilePaths[fileIndex]
			action.newPath = newBasePath

			output = append(output, action)
		}

		fileIndex++
		if fileIndex >= len(originalFilePaths) {
			break
		}
	}

	// Sanity check
	if fileIndex != len(originalFilePaths) {
		return []*FileAction{}, errors.New("not all files had a match")
	}

	return output, nil
}

func deleteTempFiles() error {
	tempFiles, err := filepath.Glob(filepath.Join(tempFolder(), "*"))
	if err != nil {
		return err
	}

	for _, p := range tempFiles {
		os.Remove(p)
	}

	return nil
}

func processFileActions(fileActions []*FileAction, dryRun bool) (bool, error) {
	hasChanges := false
	var doneActions []*FileAction

	defer func() {
		err := saveHistoryItems(doneActions)
		if err != nil {
			logError("Could not save history items: %s", err)
		}
	}()

	for _, action := range fileActions {
		hasChanges = true

		switch action.kind {

		case KIND_RENAME:

			if dryRun {
				logInfo("\"%s\"  =>  \"%s\"", action.oldPath, action.newPath)
			} else {
				logDebug("\"%s\"  =>  \"%s\"", action.oldPath, action.newPath)
				err := os.Rename(action.FullOldPath(), action.FullNewPath())
				if err != nil {
					return hasChanges, err
				}
			}
			break

		case KIND_DELETE:

			filePath := action.FullOldPath()
			if dryRun {
				logInfo("\"%s\"  =>  <Deleted>", filePath)
			} else {
				logDebug("\"%s\"  =>  <Deleted>", filePath)
				_, err := trash.MoveToTrash(filePath)
				if err != nil {
					return hasChanges, err
				}
			}
			break

		default:

			panic("Invalid action type")
			break

		}

		doneActions = append(doneActions, action)
	}

	return hasChanges, nil
}

func onExit() {
	deleteTempFiles()
	deleteOldHistoryItems(time.Now().Unix() - 60*60*24*7)
	profileClose()
}

func main() {
	minLogLevel_ = 1

	// -----------------------------------------------------------------------------------
	// Handle SIGINT (Ctrl + C)
	// -----------------------------------------------------------------------------------

	signalChan := make(chan os.Signal, 1)
	signal.Notify(signalChan, os.Interrupt, os.Kill)
	go func() {
		<-signalChan
		logInfo("Operation has been aborted.")
		onExit()
		os.Exit(2)
	}()

	defer onExit()

	// -----------------------------------------------------------------------------------
	// Parse arguments
	// -----------------------------------------------------------------------------------

	var opts CommandLineOptions
	flagParser_ = flags.NewParser(&opts, flags.HelpFlag|flags.PassDoubleDash)
	args, err := flagParser_.Parse()
	if err != nil {
		t := err.(*flags.Error).Type
		if t == flags.ErrHelp {
			printHelp()
			return
		} else {
			criticalError(err)
		}
	}

	if opts.Verbose {
		minLogLevel_ = 0
	}

	err = profileOpen()
	if err != nil {
		logError(fmt.Sprintf("%s", err))
	}

	// -----------------------------------------------------------------------------------
	// Handle selected command
	// -----------------------------------------------------------------------------------

	var commandName string
	if opts.Config {
		commandName = "config"
	} else if opts.Undo {
		commandName = "undo"
	} else if opts.Version {
		commandName = "version"
	} else {
		commandName = "rename"
	}

	var commandErr error
	switch commandName {
	case "config":
		commandErr = handleConfigCommand(&opts, args)
	case "undo":
		commandErr = handleUndoCommand(&opts, args)
	case "version":
		commandErr = handleVersionCommand(&opts, args)
	}

	if commandErr != nil {
		criticalError(commandErr)
	}

	if commandName != "rename" {
		return
	}

	filePaths, err := filePathsFromArgs(args)

	if err != nil {
		criticalError(err)
	}

	if len(filePaths) == 0 {
		criticalError(errors.New("no file to rename"))
	}

	// -----------------------------------------------------------------------------------
	// Build file list
	// -----------------------------------------------------------------------------------

	listFileContent := ""
	baseFilename := ""

	// NOTE: kr/text.Wrap returns lines separated by \n for all platforms.
	// So here hard-code \n too. Later it will be changed to \r\n for Windows.
	header := text.Wrap("Please change the filenames that need to be renamed and save the file. Lines that are not changed will be ignored (no file will be renamed), so will empty lines.", LINE_LENGTH-3)
	header += "\n"
	header += "\n" + text.Wrap("You may delete a file by putting \"//\" at the beginning of the line. Note that this operation cannot be undone (though the file can be recovered from the trash on Windows and OSX).", LINE_LENGTH-3)
	header += "\n"
	header += "\n" + text.Wrap("Please do not swap the order of lines as this is what is used to match the original filenames to the new ones. Also do not delete lines as the rename operation will be cancelled due to a mismatch between the number of filenames before and after saving the file. You may test the effect of the rename operation using the --dry-run parameter.", LINE_LENGTH-3)
	header += "\n"
	header += "\n" + text.Wrap("Caveats: "+APPNAME+" expects filenames to be reasonably sane. Filenames that include newlines or non-printable characters for example will probably not work.", LINE_LENGTH-3)

	headerLines := strings.Split(header, "\n")
	temp := ""
	for _, line := range headerLines {
		if temp != "" {
			temp += newline()
		}
		temp += "// " + line
	}
	header = temp

	for _, filePath := range filePaths {
		if listFileContent != "" {
			listFileContent += newline()
		}
		listFileContent += filepath.Base(filePath)
		baseFilename += filePath + "|"
	}

	baseFilename = stringHash(baseFilename)
	listFilePath := filepath.Join(tempFolder(), baseFilename+".files.txt")

	listFileContent = header + newline() + newline() + listFileContent
	ioutil.WriteFile(listFilePath, []byte(listFileContent), PROFILE_PERM)

	// -----------------------------------------------------------------------------------
	// Watch for changes in file list
	// -----------------------------------------------------------------------------------

	waitForFileChange := make(chan bool)
	waitForCommand := make(chan bool)

	go func(doneChan chan bool) {
		defer func() {
			doneChan <- true
		}()

		logInfo("Waiting for file list to be saved... (Press Ctrl + C to abort)")
		err := watchFile(listFilePath)
		if err != nil {
			criticalError(err)
		}
	}(waitForFileChange)

	// -----------------------------------------------------------------------------------
	// Launch text editor
	// -----------------------------------------------------------------------------------

	go func(doneChan chan bool) {
		defer func() {
			doneChan <- true
		}()

		err := editFile(listFilePath)
		if err != nil {
			criticalError(err)
		}
	}(waitForCommand)

	<-waitForCommand
	<-waitForFileChange

	// -----------------------------------------------------------------------------------
	// Check that the filenames have not been changed while the list was being edited
	// -----------------------------------------------------------------------------------

	for _, filePath := range filePaths {
		if _, err := os.Stat(filePath); os.IsNotExist(err) {
			criticalError(errors.New("Filenames have been changed or some files have been deleted or moved while the list was being edited. To avoid any data loss, the operation has been aborted. You may resume it by running the same command."))
		}
	}

	// -----------------------------------------------------------------------------------
	// Get new filenames from list file
	// -----------------------------------------------------------------------------------

	changedContent, err := ioutil.ReadFile(listFilePath)
	if err != nil {
		criticalError(err)
	}

	actions, err := fileActions(filePaths, string(changedContent))
	if err != nil {
		criticalError(err)
	}

	// -----------------------------------------------------------------------------------
	// Process the files
	// -----------------------------------------------------------------------------------

	hasChanges, err := processFileActions(actions, opts.DryRun)
	if err != nil {
		criticalError(err)
	}

	if !hasChanges {
		logDebug("No changes.")
	}
}
