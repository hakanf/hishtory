package lib

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"net/http"
	"os"
	"os/exec"
	"os/user"
	"path"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	_ "embed" // for embedding config.sh

	"github.com/glebarez/sqlite" // an alternate non-cgo-requiring sqlite driver
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"github.com/fatih/color"
	"github.com/google/uuid"
	"github.com/rodaine/table"

	"github.com/ddworken/hishtory/client/data"
	"github.com/ddworken/hishtory/shared"
)

//go:embed config.sh
var ConfigShContents string

//go:embed test_config.sh
var TestConfigShContents string

//go:embed config.zsh
var ConfigZshContents string

//go:embed test_config.zsh
var TestConfigZshContents string

var Version string = "Unknown"

func getCwd() (string, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return "", fmt.Errorf("failed to get cwd for last command: %v", err)
	}
	homedir, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("failed to get user's home directory: %v", err)
	}
	if cwd == homedir {
		return "~/", nil
	}
	if strings.HasPrefix(cwd, homedir) {
		return strings.Replace(cwd, homedir, "~", 1), nil
	}
	return cwd, nil
}

func BuildHistoryEntry(args []string) (*data.HistoryEntry, error) {
	shell := args[2]
	var entry data.HistoryEntry

	// exitCode
	exitCode, err := strconv.Atoi(args[3])
	if err != nil {
		return nil, fmt.Errorf("failed to build history entry: %v", err)
	}
	entry.ExitCode = exitCode

	// user
	user, err := user.Current()
	if err != nil {
		return nil, fmt.Errorf("failed to build history entry: %v", err)
	}
	entry.LocalUsername = user.Username

	// cwd
	cwd, err := getCwd()
	if err != nil {
		return nil, fmt.Errorf("failed to build history entry: %v", err)
	}
	entry.CurrentWorkingDirectory = cwd

	// start time
	seconds, err := parseCrossPlatformInt(args[5])
	if err != nil {
		return nil, fmt.Errorf("failed to parse start time %s as int: %v", args[5], err)
	}
	entry.StartTime = time.Unix(seconds, 0)

	// end time
	entry.EndTime = time.Now()

	// command
	if shell == "bash" {
		cmd, err := getLastCommand(args[4])
		if err != nil {
			return nil, fmt.Errorf("failed to build history entry: %v", err)
		}
		shouldBeSkipped, err := shouldSkipHiddenCommand(args[4])
		if err != nil {
			return nil, fmt.Errorf("failed to check if command was hidden: %v", err)
		}
		if shouldBeSkipped || strings.HasPrefix(cmd, " ") {
			// Don't save commands that start with a space
			return nil, nil
		}
		entry.Command = cmd
	} else if shell == "zsh" {
		cmd := strings.TrimSuffix(strings.TrimSuffix(args[4], "\n"), " ")
		if strings.HasPrefix(cmd, " ") {
			// Don't save commands that start with a space
			return nil, nil
		}
		entry.Command = cmd
	} else {
		return nil, fmt.Errorf("tried to save a hishtory entry from an unsupported shell=%#v", shell)
	}

	// hostname
	hostname, err := os.Hostname()
	if err != nil {
		return nil, fmt.Errorf("failed to build history entry: %v", err)
	}
	entry.Hostname = hostname

	return &entry, nil
}

func parseCrossPlatformInt(data string) (int64, error) {
	data = strings.TrimSuffix(data, "N")
	return strconv.ParseInt(data, 10, 64)
}

func getLastCommand(history string) (string, error) {
	return strings.SplitN(strings.SplitN(strings.TrimSpace(history), " ", 2)[1], " ", 2)[1], nil
}

func shouldSkipHiddenCommand(historyLine string) (bool, error) {
	config, err := GetConfig()
	if err != nil {
		return false, err
	}
	if config.LastSavedHistoryLine == historyLine {
		return true, nil
	}
	config.LastSavedHistoryLine = historyLine
	err = SetConfig(config)
	if err != nil {
		return false, err
	}
	return false, nil
}

func GetUserSecret() (string, error) {
	config, err := GetConfig()
	if err != nil {
		return "", err
	}
	return config.UserSecret, nil
}

func Setup(args []string) error {
	userSecret := uuid.Must(uuid.NewRandom()).String()
	if len(args) > 2 && args[2] != "" {
		userSecret = args[2]
	}
	fmt.Println("Setting secret hishtory key to " + string(userSecret))

	// Create and set the config
	var config ClientConfig
	config.UserSecret = userSecret
	config.IsEnabled = true
	config.DeviceId = uuid.Must(uuid.NewRandom()).String()
	err := SetConfig(config)
	if err != nil {
		return fmt.Errorf("failed to persist config to disk: %v", err)
	}

	// Drop all existing data
	db, err := OpenLocalSqliteDb()
	if err != nil {
		return fmt.Errorf("failed to open DB: %v", err)
	}
	db.Exec("DELETE FROM history_entries")

	// Bootstrap from remote date
	_, err = ApiGet("/api/v1/eregister?user_id=" + data.UserId(userSecret) + "&device_id=" + config.DeviceId)
	if err != nil {
		return fmt.Errorf("failed to register device with backend: %v", err)
	}

	respBody, err := ApiGet("/api/v1/ebootstrap?user_id=" + data.UserId(userSecret) + "&device_id=" + config.DeviceId)
	if err != nil {
		return fmt.Errorf("failed to bootstrap device from the backend: %v", err)
	}
	var retrievedEntries []*shared.EncHistoryEntry
	err = json.Unmarshal(respBody, &retrievedEntries)
	if err != nil {
		return fmt.Errorf("failed to load JSON response: %v", err)
	}
	for _, entry := range retrievedEntries {
		decEntry, err := data.DecryptHistoryEntry(userSecret, *entry)
		if err != nil {
			return fmt.Errorf("failed to decrypt history entry from server: %v", err)
		}
		AddToDbIfNew(db, decEntry)
	}

	return nil
}

func AddToDbIfNew(db *gorm.DB, entry data.HistoryEntry) {
	tx := db.Where("local_username = ?", entry.LocalUsername)
	tx = tx.Where("hostname = ?", entry.Hostname)
	tx = tx.Where("command = ?", entry.Command)
	tx = tx.Where("current_working_directory = ?", entry.CurrentWorkingDirectory)
	tx = tx.Where("exit_code = ?", entry.ExitCode)
	tx = tx.Where("start_time = ?", entry.StartTime)
	tx = tx.Where("end_time = ?", entry.EndTime)
	var results []data.HistoryEntry
	tx.Limit(1).Find(&results)
	if len(results) == 0 {
		db.Create(entry)
	}
}

func DisplayResults(results []*data.HistoryEntry) {
	headerFmt := color.New(color.FgGreen, color.Underline).SprintfFunc()
	tbl := table.New("Hostname", "CWD", "Timestamp", "Runtime", "Exit Code", "Command")
	tbl.WithHeaderFormatter(headerFmt)

	for _, result := range results {
		timestamp := result.StartTime.Format("Jan 2 2006 15:04:05 MST")
		duration := result.EndTime.Sub(result.StartTime).Round(time.Millisecond).String()
		tbl.AddRow(result.Hostname, result.CurrentWorkingDirectory, timestamp, duration, result.ExitCode, result.Command)
	}

	tbl.Print()
}

type ClientConfig struct {
	UserSecret           string `json:"user_secret"`
	IsEnabled            bool   `json:"is_enabled"`
	DeviceId             string `json:"device_id"`
	LastSavedHistoryLine string `json:"last_saved_history_line"`
}

func GetConfig() (ClientConfig, error) {
	homedir, err := os.UserHomeDir()
	if err != nil {
		return ClientConfig{}, fmt.Errorf("failed to retrieve homedir: %v", err)
	}
	data, err := os.ReadFile(path.Join(homedir, shared.HISHTORY_PATH, shared.CONFIG_PATH))
	if err != nil {
		files, err := ioutil.ReadDir(path.Join(homedir, shared.HISHTORY_PATH))
		if err != nil {
			return ClientConfig{}, fmt.Errorf("failed to read config file (and failed to list too): %v", err)
		}
		filenames := ""
		for _, file := range files {
			filenames += file.Name()
			filenames += ", "
		}
		return ClientConfig{}, fmt.Errorf("failed to read config file (files in ~/.hishtory/: %s): %v", filenames, err)
	}
	var config ClientConfig
	err = json.Unmarshal(data, &config)
	if err != nil {
		return ClientConfig{}, fmt.Errorf("failed to parse config file: %v", err)
	}
	return config, nil
}

func SetConfig(config ClientConfig) error {
	serializedConfig, err := json.Marshal(config)
	if err != nil {
		return fmt.Errorf("failed to serialize config: %v", err)
	}
	homedir, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("failed to retrieve homedir: %v", err)
	}
	clientDir := path.Join(homedir, shared.HISHTORY_PATH)
	err = os.MkdirAll(clientDir, 0o744)
	if err != nil {
		return fmt.Errorf("failed to create ~/.hishtory/ folder: %v", err)
	}
	err = os.WriteFile(path.Join(homedir, shared.HISHTORY_PATH, shared.CONFIG_PATH), serializedConfig, 0o600)
	if err != nil {
		return fmt.Errorf("failed to write config: %v", err)
	}
	return nil
}

func IsEnabled() (bool, error) {
	config, err := GetConfig()
	if err != nil {
		return false, err
	}
	return config.IsEnabled, nil
}

func Enable() error {
	config, err := GetConfig()
	if err != nil {
		return err
	}
	config.IsEnabled = true
	return SetConfig(config)
}

func Disable() error {
	config, err := GetConfig()
	if err != nil {
		return err
	}
	config.IsEnabled = false
	return SetConfig(config)
}

func CheckFatalError(err error) {
	if err != nil {
		_, filename, line, _ := runtime.Caller(1)
		log.Fatalf("hishtory fatal error at %s:%d: %v", filename, line, err)
	}
}

func Install() error {
	homedir, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("failed to get user's home directory: %v", err)
	}
	clientDir := path.Join(homedir, shared.HISHTORY_PATH)
	err = os.MkdirAll(clientDir, 0o744)
	if err != nil {
		return fmt.Errorf("failed to create folder for hishtory binary: %v", err)
	}
	path, err := installBinary(homedir)
	if err != nil {
		return err
	}
	err = configureBashrc(homedir, path)
	if err != nil {
		return err
	}
	err = configureZshrc(homedir, path)
	if err != nil {
		return err
	}
	_, err = GetConfig()
	if err != nil {
		// No config, so set up a new installation
		return Setup(os.Args)
	}
	return nil
}

func configureZshrc(homedir, binaryPath string) error {
	// Create the file we're going to source in our zshrc. Do this no matter what in case there are updates to it.
	zshConfigPath := path.Join(homedir, shared.HISHTORY_PATH, "config.zsh")
	configContents := ConfigZshContents
	if os.Getenv("HISHTORY_TEST") != "" {
		configContents = TestConfigZshContents
	}
	err := ioutil.WriteFile(zshConfigPath, []byte(configContents), 0o644)
	if err != nil {
		return fmt.Errorf("failed to write config.zsh file: %v", err)
	}
	// Check if we need to configure the zshrc
	zshIsConfigured, err := isZshConfigured(homedir)
	if err != nil {
		return fmt.Errorf("failed to check ~/.zshrc: %v", err)
	}
	if zshIsConfigured {
		return nil
	}
	// Add to zshrc
	f, err := os.OpenFile(path.Join(homedir, ".zshrc"), os.O_APPEND|os.O_WRONLY|os.O_CREATE, 0o644)
	if err != nil {
		return fmt.Errorf("failed to append to zshrc: %v", err)
	}
	defer f.Close()
	_, err = f.WriteString("\n# Hishtory Config:\nexport PATH=\"$PATH:" + path.Join(homedir, shared.HISHTORY_PATH) + "\"\nsource " + zshConfigPath + "\n")
	if err != nil {
		return fmt.Errorf("failed to append to zshrc: %v", err)
	}
	return nil
}

func isZshConfigured(homedir string) (bool, error) {
	_, err := os.Stat(path.Join(homedir, ".zshrc"))
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	bashrc, err := ioutil.ReadFile(path.Join(homedir, ".zshrc"))
	if err != nil {
		return false, fmt.Errorf("failed to read zshrc: %v", err)
	}
	return strings.Contains(string(bashrc), "# Hishtory Config:"), nil
}

func configureBashrc(homedir, binaryPath string) error {
	// Create the file we're going to source in our bashrc. Do this no matter what in case there are updates to it.
	bashConfigPath := path.Join(homedir, shared.HISHTORY_PATH, "config.sh")
	configContents := ConfigShContents
	if os.Getenv("HISHTORY_TEST") != "" {
		configContents = TestConfigShContents
	}
	err := ioutil.WriteFile(bashConfigPath, []byte(configContents), 0o644)
	if err != nil {
		return fmt.Errorf("failed to write config.sh file: %v", err)
	}
	// Check if we need to configure the bashrc
	bashIsConfigured, err := isBashConfigured(homedir)
	if err != nil {
		return fmt.Errorf("failed to check ~/.bashrc: %v", err)
	}
	if bashIsConfigured {
		return nil
	}
	// Add to bashrc
	f, err := os.OpenFile(path.Join(homedir, ".bashrc"), os.O_APPEND|os.O_WRONLY|os.O_CREATE, 0o644)
	if err != nil {
		return fmt.Errorf("failed to append to bashrc: %v", err)
	}
	defer f.Close()
	_, err = f.WriteString("\n# Hishtory Config:\nexport PATH=\"$PATH:" + path.Join(homedir, shared.HISHTORY_PATH) + "\"\nsource " + bashConfigPath + "\n")
	if err != nil {
		return fmt.Errorf("failed to append to bashrc: %v", err)
	}
	return nil
}

func isBashConfigured(homedir string) (bool, error) {
	_, err := os.Stat(path.Join(homedir, ".bashrc"))
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	bashrc, err := ioutil.ReadFile(path.Join(homedir, ".bashrc"))
	if err != nil {
		return false, fmt.Errorf("failed to read bashrc: %v", err)
	}
	return strings.Contains(string(bashrc), "# Hishtory Config:"), nil
}

func installBinary(homedir string) (string, error) {
	clientPath, err := exec.LookPath("hishtory")
	if err != nil {
		clientPath = path.Join(homedir, shared.HISHTORY_PATH, "hishtory")
	}
	if _, err := os.Stat(clientPath); err == nil {
		err = syscall.Unlink(clientPath)
		if err != nil {
			return "", fmt.Errorf("failed to unlink %s for install: %v", clientPath, err)
		}
	}
	err = copyFile(os.Args[0], clientPath)
	if err != nil {
		return "", fmt.Errorf("failed to copy hishtory binary to $PATH: %v", err)
	}
	err = os.Chmod(clientPath, 0o700)
	if err != nil {
		return "", fmt.Errorf("failed to set permissions on hishtory binary: %v", err)
	}
	return clientPath, nil
}

func copyFile(src, dst string) error {
	sourceFileStat, err := os.Stat(src)
	if err != nil {
		return err
	}

	if !sourceFileStat.Mode().IsRegular() {
		return fmt.Errorf("%s is not a regular file", src)
	}

	source, err := os.Open(src)
	if err != nil {
		return err
	}
	defer source.Close()

	destination, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer destination.Close()
	_, err = io.Copy(destination, source)
	return err
}

func Update() error {
	// Download the binary
	respBody, err := ApiGet("/api/v1/download")
	if err != nil {
		return fmt.Errorf("failed to download update info: %v", err)
	}
	var downloadData shared.UpdateInfo
	err = json.Unmarshal(respBody, &downloadData)
	if err != nil {
		return fmt.Errorf("failed to parse update info: %v", err)
	}
	if downloadData.Version == "v0."+Version {
		fmt.Printf("Latest version (v0.%s) is already installed\n", Version)
		return nil
	}
	err = downloadFiles(downloadData)
	if err != nil {
		return err
	}

	// Verify the SLSA attestation
	err = verifyBinary("/tmp/hishtory-client", "/tmp/hishtory-client.intoto.jsonl", downloadData.Version+"-"+runtime.GOOS+"-"+runtime.GOARCH)
	if err != nil {
		return fmt.Errorf("failed to verify SLSA provenance of the updated binary, aborting update: %v", err)
	}

	// Unlink the existing binary so we can overwrite it even though it is still running
	if runtime.GOOS == "linux" {
		homedir, err := os.UserHomeDir()
		if err != nil {
			return fmt.Errorf("failed to get user's home directory: %v", err)
		}
		err = syscall.Unlink(path.Join(homedir, shared.HISHTORY_PATH, "hishtory"))
		if err != nil {
			return fmt.Errorf("failed to unlink %s for update: %v", path.Join(homedir, shared.HISHTORY_PATH, "hishtory"), err)
		}
	}

	// Install the new one
	cmd := exec.Command("chmod", "+x", "/tmp/hishtory-client")
	var stdout bytes.Buffer
	cmd.Stdout = &stdout
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	err = cmd.Run()
	if err != nil {
		return fmt.Errorf("failed to chmod +x the update (out=%#v, err=%#v): %v", stdout.String(), stderr.String(), err)
	}
	cmd = exec.Command("/tmp/hishtory-client", "install")
	stdout = bytes.Buffer{}
	stderr = bytes.Buffer{}
	err = cmd.Run()
	if err != nil {
		return fmt.Errorf("failed to install update (out=%#v, err=%#v): %v", stdout.String(), stderr.String(), err)
	}
	fmt.Printf("Successfully updated hishtory from v0.%s to %s\n", Version, downloadData.Version)
	return nil
}

func downloadFiles(updateInfo shared.UpdateInfo) error {
	clientUrl := ""
	clientProvenanceUrl := ""
	if runtime.GOOS == "linux" && runtime.GOARCH == "amd64" {
		clientUrl = updateInfo.LinuxAmd64Url
		clientProvenanceUrl = updateInfo.LinuxAmd64AttestationUrl
	} else if runtime.GOOS == "darwin" && runtime.GOARCH == "amd64" {
		clientUrl = updateInfo.DarwinAmd64Url
		clientProvenanceUrl = updateInfo.DarwinAmd64AttestationUrl
	} else if runtime.GOOS == "darwin" && runtime.GOARCH == "arm64" {
		clientUrl = updateInfo.DarwinArm64Url
		clientProvenanceUrl = updateInfo.DarwinArm64AttestationUrl
	} else {
		return fmt.Errorf("no update info found for GOOS=%s, GOARCH=%s", runtime.GOOS, runtime.GOARCH)
	}
	err := downloadFile("/tmp/hishtory-client", clientUrl)
	if err != nil {
		return err
	}
	err = downloadFile("/tmp/hishtory-client.intoto.jsonl", clientProvenanceUrl)
	if err != nil {
		return err
	}
	return nil
}

func downloadFile(filename, url string) error {
	resp, err := http.Get(url)
	if err != nil {
		return fmt.Errorf("failed to download file at %s to %s: %v", url, filename, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return fmt.Errorf("failed to download file at %s due to resp_code=%d", url, resp.StatusCode)
	}

	out, err := os.Create(filename)
	if err != nil {
		return fmt.Errorf("failed to save file to %s: %v", filename, err)
	}
	defer out.Close()

	_, err = io.Copy(out, resp.Body)

	return err
}

func getServerHostname() string {
	if server := os.Getenv("HISHTORY_SERVER"); server != "" {
		return server
	}
	return "https://api.hishtory.dev"
}

var (
	hishtoryLogger *log.Logger
	getLoggerOnce  sync.Once
)

func getLogger() *log.Logger {
	getLoggerOnce.Do(func() {
		homedir, err := os.UserHomeDir()
		if err != nil {
			panic(fmt.Errorf("failed to get user's home directory: %v", err))
		}
		f, err := os.OpenFile(path.Join(homedir, shared.HISHTORY_PATH, "hishtory.log"), os.O_RDWR|os.O_CREATE|os.O_APPEND, 0o660)
		if err != nil {
			panic(fmt.Errorf("failed to open hishtory.log: %v", err))
		}
		// Purposefully not closing the file. Yes, this is a dangling file handle. But hishtory is short lived so this is okay.
		hishtoryLogger = log.New(f, "\n", log.LstdFlags|log.Lshortfile)
	})
	return hishtoryLogger
}

func OpenLocalSqliteDb() (*gorm.DB, error) {
	homedir, err := os.UserHomeDir()
	if err != nil {
		return nil, fmt.Errorf("failed to get user's home directory: %v", err)
	}
	err = os.MkdirAll(path.Join(homedir, shared.HISHTORY_PATH), 0o744)
	if err != nil {
		return nil, fmt.Errorf("failed to create ~/.hishtory dir: %v", err)
	}
	hishtoryLogger := getLogger()
	newLogger := logger.New(
		hishtoryLogger,
		logger.Config{
			SlowThreshold:             100 * time.Millisecond,
			LogLevel:                  logger.Warn,
			IgnoreRecordNotFoundError: false,
			Colorful:                  false,
		},
	)
	db, err := gorm.Open(sqlite.Open(path.Join(homedir, shared.HISHTORY_PATH, shared.DB_PATH)), &gorm.Config{SkipDefaultTransaction: true, Logger: newLogger})
	if err != nil {
		return nil, fmt.Errorf("failed to connect to the DB: %v", err)
	}
	tx, err := db.DB()
	if err != nil {
		return nil, err
	}
	err = tx.Ping()
	if err != nil {
		return nil, err
	}
	db.AutoMigrate(&data.HistoryEntry{})
	db.Exec("PRAGMA journal_mode = WAL")
	return db, nil
}

func ApiGet(path string) ([]byte, error) {
	start := time.Now()
	resp, err := http.Get(getServerHostname() + path)
	if err != nil {
		return nil, fmt.Errorf("failed to GET %s: %v", path, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("failed to GET %s: status_code=%d", path, resp.StatusCode)
	}
	respBody, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read response body from GET %s: %v", path, err)
	}
	duration := time.Since(start)
	getLogger().Printf("ApiGet(%#v): %s\n", path, duration.String())
	return respBody, nil
}

func ApiPost(path, contentType string, data []byte) ([]byte, error) {
	start := time.Now()
	resp, err := http.Post(getServerHostname()+path, contentType, bytes.NewBuffer(data))
	if err != nil {
		return nil, fmt.Errorf("failed to POST %s: %v", path, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("failed to POST %s: status_code=%d", path, resp.StatusCode)
	}
	respBody, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read response body from POST %s: %v", path, err)
	}
	duration := time.Since(start)
	getLogger().Printf("ApiPost(%#v): %s\n", path, duration.String())
	return respBody, nil
}
