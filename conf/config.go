package conf

import (
	"fmt"
	"log"
	"os"
	"path/filepath"

	"github.com/BurntSushi/toml"
)

var config *ComputeNode

// ComputeNode is a compute node config
type ComputeNode struct {
	API      API
	LAG      LAG
	MCS      MCS
	Registry Registry
}

type API struct {
	Port          int
	MultiAddress  string
	RedisUrl      string
	RedisPassword string
	Domain        string
	NodeName      string
}

type LAG struct {
	ServerUrl   string
	AccessToken string
}

type MCS struct {
	ApiKey        string
	AccessToken   string
	BucketName    string
	Network       string
	FileCachePath string
}

type Registry struct {
	ServerAddress string
	UserName      string
	Password      string
}

func InitConfig() error {
	currentDir, _ := os.Getwd()
	configFile := filepath.Join(currentDir, "config.toml")

	if metaData, err := toml.DecodeFile(configFile, &config); err != nil {
		return fmt.Errorf("failed load config file, path: %s, error: %w", configFile, err)
	} else {
		if !requiredFieldsAreGiven(metaData) {
			log.Fatal("Required fields not given")
		}
	}
	return nil
}

func GetConfig() *ComputeNode {
	return config
}

func requiredFieldsAreGiven(metaData toml.MetaData) bool {
	requiredFields := [][]string{
		{"API"},
		{"LAG"},
		{"MCS"},
		{"Registry"},

		{"API", "MultiAddress"},
		{"API", "Domain"},
		{"API", "RedisUrl"},

		{"LAG", "ServerUrl"},
		{"LAG", "AccessToken"},

		{"MCS", "ApiKey"},
		{"MCS", "AccessToken"},
		{"MCS", "BucketName"},
		{"MCS", "Network"},
		{"MCS", "FileCachePath"},
	}

	for _, v := range requiredFields {
		if !metaData.IsDefined(v...) {
			log.Fatal("Required fields ", v)
		}
	}

	return true
}
