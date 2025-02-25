package github

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"strings"

	"github.com/bradleyfalzon/ghinstallation"
	"github.com/go-logr/logr"
	"github.com/google/go-github/v39/github"
	"github.com/patrickmn/go-cache"
	"github.com/prometheus/client_golang/prometheus"
	v1alpha1 "github.com/skillz/opvic/agent/api/v1alpha1"
	"github.com/skillz/opvic/utils"
	"golang.org/x/oauth2"
)

var (
	rateLimitRemaining = prometheus.NewGauge(
		prometheus.GaugeOpts{
			Namespace: "opvic_provider_github",
			Name:      "rate_limit_remaining",
			Help:      "The number of requests remaining in the current rate limit window.",
		},
	)
)

// Config contains configuration for Github provider
type Config struct {
	AppID             int64
	AppInstallationID int64
	AppPrivateKey     string
	Token             string
}

// Provider is a github provider for getting remote versions from Github
type Provider struct {
	client *github.Client
	ctx    context.Context
	cache  *cache.Cache
	log    logr.Logger
}

func init() {
	prometheus.MustRegister(rateLimitRemaining)
}

func (c *Config) NewProvider(ctx context.Context, cache *cache.Cache, logger logr.Logger) (*Provider, error) {
	var transport http.RoundTripper
	var client *github.Client
	if c.Token != "" {
		transport = oauth2.NewClient(context.Background(), oauth2.StaticTokenSource(&oauth2.Token{AccessToken: c.Token})).Transport
	} else if c.AppID != 0 && c.AppInstallationID != 0 && c.AppPrivateKey != "" {
		var tr *ghinstallation.Transport
		tr = nil

		if _, err := os.Stat(c.AppPrivateKey); err == nil {
			tr, err = ghinstallation.NewKeyFromFile(http.DefaultTransport, c.AppID, c.AppInstallationID, c.AppPrivateKey)
			if err != nil {
				return nil, fmt.Errorf("authentication failed: using private key from file %s: %v", c.AppPrivateKey, err)
			}
		} else if c.AppPrivateKey != "" {
			tr, err = ghinstallation.New(http.DefaultTransport, c.AppID, c.AppInstallationID, []byte(c.AppPrivateKey))
			if err != nil {
				return nil, fmt.Errorf("authentication failed: using private key: %v", err)
			}
		}

		transport = tr
	}
	if transport != nil {
		httpClient := &http.Client{Transport: transport}
		client = github.NewClient(httpClient)
	} else {
		logger.V(1).Info("no authentication provided. You might encounter Github API rate limiting issues.")
		client = github.NewClient(nil)
	}

	// Check the rate limit and set it as metrics on startup
	limit, _, err := client.RateLimits(ctx)
	if err != nil {
		return nil, err
	}
	logger.V(1).Info("rate limit", "remaining", limit.Core.Remaining)
	rateLimitRemaining.Set(float64(limit.Core.Remaining))

	return &Provider{
		client: client,
		ctx:    ctx,
		cache:  cache,
		log:    logger,
	}, nil
}

func (p *Provider) getCacheValue(key string) (interface{}, bool) {
	return p.cache.Get(key)
}

func (p *Provider) setCacheValue(key string, value interface{}) {
	p.cache.Set(key, value, cache.DefaultExpiration)
}

func releasesCacheKey(repo string) string {
	return fmt.Sprintf("github/%s/releases", repo)
}

func tagsCacheKey(repo string) string {
	return fmt.Sprintf("github/%s/tags", repo)
}

func (p *Provider) getReleases(repo string) ([]*github.RepositoryRelease, error) {
	log := p.log.WithValues("repo", repo)
	var releases []*github.RepositoryRelease
	if r, ok := p.getCacheValue(releasesCacheKey(repo)); !ok {
		log.V(1).Info("getting releases")
		owner, name, err := splitRepo(repo)
		if err != nil {
			return nil, err
		}
		// get releases by pagination (max 100)
		opt := &github.ListOptions{
			PerPage: 100,
		}
		for {
			releasesPage, resp, err := p.client.Repositories.ListReleases(p.ctx, owner, name, opt)
			if err != nil {
				return nil, err
			}
			releases = append(releases, releasesPage...)
			if resp.NextPage == 0 {
				break
			}
			opt.Page = resp.NextPage
		}
		p.setCacheValue(releasesCacheKey(repo), releases)
	} else {
		log.V(1).Info("found releases in cache")
		releases = r.([]*github.RepositoryRelease)
	}
	return releases, nil
}

func (p *Provider) getTags(repo string) ([]*github.RepositoryTag, error) {
	log := p.log.WithValues("repo", repo)
	var tags []*github.RepositoryTag
	if t, ok := p.getCacheValue(tagsCacheKey(repo)); !ok {
		log.V(1).Info("getting tags")
		owner, name, err := splitRepo(repo)
		if err != nil {
			return nil, err
		}
		// get releases by pagination (max 100)
		opt := &github.ListOptions{
			PerPage: 100,
		}
		for {
			tagsPage, resp, err := p.client.Repositories.ListTags(p.ctx, owner, name, opt)
			if err != nil {
				return nil, err
			}
			tags = append(tags, tagsPage...)
			if resp.NextPage == 0 {
				break
			}
			opt.Page = resp.NextPage
		}
		p.setCacheValue(tagsCacheKey(repo), tags)
	} else {
		log.V(1).Info("found tags in cache")
		tags = t.([]*github.RepositoryTag)
	}
	return tags, nil
}

func (p *Provider) getVersionsFromReleases(conf v1alpha1.RemoteVersion) ([]string, error) {
	var matchedVersions []string
	var versions []string
	releases, err := p.getReleases(conf.Repo)
	if err != nil {
		return nil, err
	}
	for _, release := range releases {
		if release.GetTagName() == "" {
			continue
		}
		matched, v := utils.MatchPattern(conf.Extraction.Regex.Pattern, conf.Extraction.Regex.Result, release.GetName())
		if matched {
			matchedVersions = append(matchedVersions, v)
		}
	}
	if conf.Constraint == "" {
		return matchedVersions, nil
	} else {
		for _, version := range matchedVersions {
			meet, err := utils.MeetConstraint(conf.Constraint, version)
			if err != nil {
				return nil, err
			}
			if meet {
				versions = append(versions, version)
			}
		}
	}
	return versions, nil
}

func (p *Provider) getVersionsFromTags(conf v1alpha1.RemoteVersion) ([]string, error) {
	var matchedVersions []string
	var versions []string
	tags, err := p.getTags(conf.Repo)
	if err != nil {
		return nil, err
	}
	for _, tag := range tags {
		if tag.GetName() == "" {
			continue
		}
		matched, v := utils.MatchPattern(conf.Extraction.Regex.Pattern, conf.Extraction.Regex.Result, tag.GetName())
		if matched {
			matchedVersions = append(matchedVersions, v)
		}
	}
	if conf.Constraint == "" {
		return matchedVersions, nil
	} else {
		for _, version := range matchedVersions {
			meet, err := utils.MeetConstraint(conf.Constraint, version)
			if err != nil {
				return nil, err
			}
			if meet {
				versions = append(versions, version)
			}
		}
	}
	return versions, nil
}

func (p *Provider) GetVersions(conf v1alpha1.RemoteVersion) ([]string, error) {
	// Check the rate limit and set it as metrics
	limit, _, err := p.client.RateLimits(p.ctx)
	if err != nil {
		return nil, err
	}
	p.log.V(1).Info("rate limit", "remaining", limit.Core.Remaining)
	rateLimitRemaining.Set(float64(limit.Core.Remaining))

	if conf.Strategy == v1alpha1.GithubStrategyReleases {
		return p.getVersionsFromReleases(conf)
	} else if conf.Strategy == v1alpha1.GithubStrategyTags {
		return p.getVersionsFromTags(conf)
	}
	return nil, fmt.Errorf("strategy %s is not supported", conf.Strategy)
}

func splitRepo(repo string) (owner string, name string, err error) {
	parts := strings.SplitN(repo, "/", 2)
	if len(parts) != 2 {
		return "", "", fmt.Errorf("invalid repo: %s. it must be in the format of: owner/name", repo)
	}
	return parts[0], parts[1], nil
}
