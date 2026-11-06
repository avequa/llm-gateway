// Package config загружает и проверяет YAML-конфиг шлюза: апстримы, алиасы моделей и настройки сервера
package config

import (
	"bytes"
	"errors"
	"fmt"
	"net/url"
	"os"
	"slices"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"
)

// Значения по умолчанию
const (
	DefaultAddr            = "127.0.0.1:4000"
	DefaultShutdownTimeout = 15 * time.Second
	DefaultTimeout         = 120 * time.Second
	DefaultMaxConcurrent   = 4
)

// Config - весь конфиг шлюза
type Config struct {
	Server    Server              `yaml:"server"`
	Upstreams map[string]Upstream `yaml:"upstreams"`
	Models    map[string]Model    `yaml:"models"`
}

// Server - настройки HTTP-сервера шлюза
type Server struct {
	Addr            string        `yaml:"addr"`
	ShutdownTimeout time.Duration `yaml:"shutdown_timeout"`
}

// Upstream - OpenAI-совместимый бэкенд: Ollama, vLLM, llama.cpp или облачный API
type Upstream struct {
	BaseURL       string        `yaml:"base_url"`
	APIKeyEnv     string        `yaml:"api_key_env"`
	Timeout       time.Duration `yaml:"timeout"`
	MaxConcurrent int           `yaml:"max_concurrent"`
	APIKey        string        `yaml:"-"`
}

// Model - алиас модели, который видят агенты, и цепочка целей для fallback по порядку
type Model struct {
	Targets []Target `yaml:"targets"`
}

// Target - конкретная модель на конкретном апстриме
type Target struct {
	Upstream string `yaml:"upstream"`
	Model    string `yaml:"model"`
}

// Load - читает конфиг из файла, подставляет значения по умолчанию, ключи API берет из окружения через getenv
func Load(path string, getenv func(string) string) (Config, error) {
	data, err := os.ReadFile(path) //nolint:gosec // путь к конфигу задает оператор шлюза
	if err != nil {
		return Config{}, fmt.Errorf("read config: %w", err)
	}
	return Parse(data, getenv)
}

// Parse - разбирает YAML строго: неизвестное поле (опечатка) - ошибка, а не тихий пропуск
func Parse(data []byte, getenv func(string) string) (Config, error) {
	var cfg Config
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	if err := decoder.Decode(&cfg); err != nil {
		return Config{}, fmt.Errorf("parse config: %w", err)
	}

	cfg.applyDefaults()
	if err := cfg.validate(); err != nil {
		return Config{}, fmt.Errorf("invalid config: %w", err)
	}
	if err := cfg.resolveKeys(getenv); err != nil {
		return Config{}, fmt.Errorf("invalid config: %w", err)
	}
	return cfg, nil
}

// applyDefaults - заполняет незаданные поля значениями по умолчанию
func (c *Config) applyDefaults() {
	if c.Server.Addr == "" {
		c.Server.Addr = DefaultAddr
	}
	if c.Server.ShutdownTimeout == 0 {
		c.Server.ShutdownTimeout = DefaultShutdownTimeout
	}
	for name, u := range c.Upstreams {
		if u.Timeout == 0 {
			u.Timeout = DefaultTimeout
		}
		if u.MaxConcurrent == 0 {
			u.MaxConcurrent = DefaultMaxConcurrent
		}
		u.BaseURL = strings.TrimSuffix(u.BaseURL, "/")
		c.Upstreams[name] = u
	}
}

// validate - проверяет весь конфиг и возвращает все найденные ошибки сразу
func (c *Config) validate() error {
	var errs []error
	if c.Server.ShutdownTimeout < 0 {
		errs = append(errs, errors.New("server.shutdown_timeout must be positive"))
	}
	if len(c.Upstreams) == 0 {
		errs = append(errs, errors.New("at least one upstream is required"))
	}
	if len(c.Models) == 0 {
		errs = append(errs, errors.New("at least one model alias is required"))
	}

	for _, name := range c.UpstreamNames() {
		errs = append(errs, c.Upstreams[name].validate(name)...)
	}
	for _, alias := range c.Aliases() {
		errs = append(errs, c.validateModel(alias)...)
	}
	return errors.Join(errs...)
}

// validate - проверяет один апстрим
func (u Upstream) validate(name string) []error {
	var errs []error
	parsed, err := url.Parse(u.BaseURL)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
		errs = append(errs, fmt.Errorf("upstreams.%s.base_url %q must be an http(s) URL", name, u.BaseURL))
	}
	if u.Timeout < 0 {
		errs = append(errs, fmt.Errorf("upstreams.%s.timeout must be positive", name))
	}
	if u.MaxConcurrent < 0 {
		errs = append(errs, fmt.Errorf("upstreams.%s.max_concurrent must be positive", name))
	}
	return errs
}

// validateModel - проверяет один алиас: есть цели, каждая ссылается на известный апстрим и задает модель
func (c *Config) validateModel(alias string) []error {
	model := c.Models[alias]
	if len(model.Targets) == 0 {
		return []error{fmt.Errorf("models.%s needs at least one target", alias)}
	}

	var errs []error
	for i, t := range model.Targets {
		if _, ok := c.Upstreams[t.Upstream]; !ok {
			errs = append(errs, fmt.Errorf("models.%s.targets[%d]: unknown upstream %q", alias, i, t.Upstream))
		}
		if t.Model == "" {
			errs = append(errs, fmt.Errorf("models.%s.targets[%d]: model is required", alias, i))
		}
	}
	return errs
}

// resolveKeys - читает ключи API из переменных окружения, сами ключи в файле конфига не хранятся
func (c *Config) resolveKeys(getenv func(string) string) error {
	var errs []error
	for _, name := range c.UpstreamNames() {
		u := c.Upstreams[name]
		if u.APIKeyEnv == "" {
			continue
		}
		u.APIKey = getenv(u.APIKeyEnv)
		if u.APIKey == "" {
			errs = append(errs, fmt.Errorf("upstreams.%s: environment variable %s is empty", name, u.APIKeyEnv))
		}
		c.Upstreams[name] = u
	}
	return errors.Join(errs...)
}

// Aliases - имена алиасов по алфавиту
func (c *Config) Aliases() []string {
	return sortedKeys(c.Models)
}

// UpstreamNames - имена апстримов по алфавиту
func (c *Config) UpstreamNames() []string {
	return sortedKeys(c.Upstreams)
}

// sortedKeys - ключи map по алфавиту
func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}
