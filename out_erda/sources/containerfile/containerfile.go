package containerfile

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"
	"github.com/sirupsen/logrus"
)

const configJson = "config.v2.json"

type DockerContainerID string

type DockerContainerInfo struct {
	ID     DockerContainerID
	Name   string
	EnvMap map[string]string
	Labels map[string]string

	configFilePath string // for debug
	lastUsed       time.Time
}

type dockerConfigV2 struct {
	ID     DockerContainerID `json:"ID"`
	Name   string            `json:"Name"`
	Config struct {
		Env    []string          `json:"Env"`
		Labels map[string]string `json:"Labels"`
	} `json:"Config"`
}

type Config struct {
	RootPath           string
	EnvIncludeList     []string
	SyncInterval       time.Duration
	MaxExpiredDuration time.Duration
}

type ContainerInfoCenter struct {
	rootPath           string
	globPattern        string
	syncInterval       time.Duration
	envKeyInclude      map[string]struct{}
	maxExpiredDuration time.Duration

	mu      sync.RWMutex
	done    chan struct{}
	watcher *fsnotify.Watcher
	// todo should not exported
	// TODO DockerContainerID超过一段时间内未被访问过，才会删除
	Data map[DockerContainerID]DockerContainerInfo
}

func NewContainerInfoCenter(cfg Config) *ContainerInfoCenter {
	return &ContainerInfoCenter{
		syncInterval:       cfg.SyncInterval,
		rootPath:           cfg.RootPath,
		globPattern:        filepath.Join(cfg.RootPath, "*", configJson),
		envKeyInclude:      listToMap(cfg.EnvIncludeList),
		maxExpiredDuration: cfg.MaxExpiredDuration,
		Data:               make(map[DockerContainerID]DockerContainerInfo),
		done:               make(chan struct{}),
	}
}

func (ci *ContainerInfoCenter) Init() error {
	err := ci.initWatcher()
	if err != nil {
		return fmt.Errorf("init watcher: %w", err)
	}
	err = ci.scan()
	if err != nil {
		return fmt.Errorf("init scan: %w", err)
	}
	return nil
}

func (ci *ContainerInfoCenter) Start() {
	go ci.syncWithInterval()
	go ci.watchFileChange()
}

func (ci *ContainerInfoCenter) Close() error {
	close(ci.done)
	return ci.watcher.Close()
}

func (ci *ContainerInfoCenter) syncWithInterval() {
	ticker := time.NewTicker(ci.syncInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			err := ci.scan()
			if err != nil {
				logrus.Errorf("sync scan failed: %s", err)
			}
		case <-ci.done:
			return
		}
	}
}

func eventString(op fsnotify.Op) string {
	switch {
	case op&fsnotify.Remove == fsnotify.Remove:
		return "REMOVE"
	case op&fsnotify.Rename == fsnotify.Rename:
		return "RENAME"
	case op&fsnotify.Create == fsnotify.Create:
		return "CREATE"
	}
	return ""
}

func (ci *ContainerInfoCenter) watchFileChange() {
	for {
		select {
		case event, ok := <-ci.watcher.Events:
			if !ok {
				return
			}
			if (event.Op & fsnotify.Create) == fsnotify.Create {
				f := filepath.Join(event.Name, configJson)
				time.Sleep(time.Second) // wait for flushing
				dinfo, err := ci.readConfigFile(f)
				if err != nil {
					logrus.Errorf("readConfigFile event<%s> fialed: %s", event.Name, err)
					continue
				}
				ci.mu.Lock()
				ci.Data[dinfo.ID] = dinfo
				ci.mu.Unlock()
				logrus.Infof("inotify: event<%s> created. load file: %s success!", event.Name, f)
			}
		case event, ok := <-ci.watcher.Errors:
			if !ok {
				return
			}
			logrus.Errorf("error event received: %s", event.Error())
		case <-ci.done:
			return
		}
	}
}

func (ci *ContainerInfoCenter) initWatcher() error {
	w, err := fsnotify.NewWatcher()
	if err != nil {
		return fmt.Errorf("create fs watcher failed")
	}
	err = w.Add(ci.rootPath)
	if err != nil {
		return fmt.Errorf("add dir: %w", err)
	}
	ci.watcher = w
	return nil
}

func (ci *ContainerInfoCenter) scan() error {
	files, err := filepath.Glob(ci.globPattern)
	if err != nil {
		return err
	}
	data := make(map[DockerContainerID]DockerContainerInfo, len(files))
	for _, f := range files {
		dinfo, err := ci.readConfigFile(f)
		if err != nil {
			return err
		}
		data[dinfo.ID] = dinfo
	}

	now := time.Now()
	ci.mu.RLock()
	for k, v := range ci.Data {
		if _, ok := data[k]; !ok && now.Sub(v.lastUsed) <= ci.maxExpiredDuration {
			data[k] = v
		}
	}
	ci.mu.RUnlock()

	ci.mu.Lock()
	ci.Data = data
	ci.mu.Unlock()
	return nil
}

func (ci *ContainerInfoCenter) readConfigFile(f string) (DockerContainerInfo, error) {
	buf, err := os.ReadFile(f)
	if err != nil {
		return DockerContainerInfo{}, fmt.Errorf("read file %s failed: %w", f, err)
	}
	tmp := dockerConfigV2{}
	err = json.Unmarshal(buf, &tmp)
	if err != nil {
		return DockerContainerInfo{}, fmt.Errorf("unmarshal filed %s fialed: %w", f, err)
	}
	di := ci.convert(tmp)
	di.configFilePath = f
	return di, nil
}

func (ci *ContainerInfoCenter) convert(src dockerConfigV2) DockerContainerInfo {
	envmap := make(map[string]string)
	for _, item := range src.Config.Env {
		idx := strings.Index(item, "=")
		key, val := item[:idx], item[idx+1:]
		if _, ok := ci.envKeyInclude[key]; ok {
			envmap[key] = val
		}
	}
	return DockerContainerInfo{
		ID:       src.ID,
		Name:     src.Name,
		EnvMap:   envmap,
		Labels:   src.Config.Labels,
		lastUsed: time.Now(),
	}
}

func (ci *ContainerInfoCenter) GetInfoByContainerID(cid string) (DockerContainerInfo, bool) {
	ci.mu.RLock()
	defer ci.mu.RUnlock()
	res, ok := ci.Data[DockerContainerID(cid)]
	if ok {
		res.lastUsed = time.Now()
	}
	return res, ok
}

func listToMap(list []string) map[string]struct{} {
	res := make(map[string]struct{}, len(list))
	for _, item := range list {
		res[item] = struct{}{}
	}
	return res
}
