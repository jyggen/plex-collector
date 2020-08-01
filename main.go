package main

import (
	"errors"
	"fmt"
	"github.com/jyggen/go-plex-client"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/urfave/cli/v2"
	"log"
	"net/http"
	"os"
	"strconv"
	"time"
)

func Contains(a []string, x string) bool {
	for _, n := range a {
		if x == n {
			return true
		}
	}
	return false
}

type MediaItem struct {
	id                   int
	audioChannels        int
	audioCodec           string
	grandParentRatingKey string
	mediaType            string
	parentRatingKey      string
	sectionKey           string
	size                 int
	videoCodec           string
	videoResolution      string
}

func (m *MediaItem) Diff(x *MediaItem) bool {
	if m.audioChannels != x.audioChannels {
		return true
	}

	if m.audioCodec != x.audioCodec {
		return true
	}

	if m.videoCodec != x.videoCodec {
		return true
	}

	if m.videoResolution != x.videoResolution {
		return true
	}

	return false
}

func (m *MediaItem) Gauge(gauge *prometheus.GaugeVec) prometheus.Gauge {
	return gauge.With(prometheus.Labels{
		"audio_channels":   strconv.Itoa(m.audioChannels),
		"audio_codec":      m.audioCodec,
		"media_type":       m.mediaType,
		"video_codec":      m.videoCodec,
		"video_resolution": m.videoResolution,
	})
}

type Collector struct {
	client             *plex.Plex
	lastRun            time.Time
	mediaItems         []*MediaItem
	skippedSectionKeys []string
}

func (c *Collector) Collect() error {
	c.skippedSectionKeys = make([]string, 0)

	// Generate a new last run straight away to avoid edge cases.
	newLastRun := time.Now()
	newMediaItems := make([]*MediaItem, 0)

	libraries, err := c.client.GetLibraries()

	if err != nil {
		return err
	}

	for _, library := range libraries.MediaContainer.Directory {
		updatedAt := time.Unix(int64(library.UpdatedAt), 0)

		if updatedAt.Before(c.lastRun) {
			c.skippedSectionKeys = append(c.skippedSectionKeys, library.Key)
			continue
		}

		content, err := c.client.GetLibraryContent(library.Key, "")

		if err != nil {
			return err
		}

		mediaItems, err := c.analyzeItems(content.MediaContainer.MediaContainer)

		if err != nil {
			return err
		}

		newMediaItems = append(newMediaItems, mediaItems...)
	}

	oldMediaItemsMap := make(map[int]*MediaItem, len(c.mediaItems))

	for _, mediaItem := range c.mediaItems {
		oldMediaItemsMap[mediaItem.id] = mediaItem
	}

	newMediaItemsMap := make(map[int]*MediaItem, 0)
	added, updated, removed := 0, 0, 0

	for _, mediaItem := range newMediaItems {
		newMediaItemsMap[mediaItem.id] = mediaItem

		if _, ok := oldMediaItemsMap[mediaItem.id]; !ok {
			mediaItem.Gauge(mediaItemsCount).Inc()
			mediaItem.Gauge(mediaItemsBytes).Add(float64(mediaItem.size))
			added++
			continue
		}

		oldItem := oldMediaItemsMap[mediaItem.id]

		if mediaItem.Diff(oldItem) {
			oldItem.Gauge(mediaItemsCount).Dec()
			mediaItem.Gauge(mediaItemsCount).Inc()
			oldItem.Gauge(mediaItemsBytes).Sub(float64(oldItem.size))
			mediaItem.Gauge(mediaItemsBytes).Add(float64(mediaItem.size))
			updated++
		}

		delete(oldMediaItemsMap, mediaItem.id)
	}

	for _, mediaItem := range oldMediaItemsMap {
		if Contains(c.skippedSectionKeys, mediaItem.sectionKey) {
			newMediaItems = append(newMediaItems, mediaItem)
			continue
		}

		mediaItem.Gauge(mediaItemsCount).Dec()
		mediaItem.Gauge(mediaItemsBytes).Sub(float64(mediaItem.size))

		removed++
	}

	c.mediaItems = newMediaItems
	c.lastRun = newLastRun

	log.Printf("Collection of %d media items finished. Added %d, updated %d, and removed %d.\n", len(c.mediaItems), added, updated, removed)

	return nil
}

func (c *Collector) analyzeItems(container plex.MediaContainer) ([]*MediaItem, error) {
	newMediaItems := make([]*MediaItem, 0)

	for _, item := range container.Metadata {
		if item.Type == "artist" || item.Type == "album" {
			continue
		} else if item.Type == "show" || item.Type == "season" {
			content, err := c.client.GetMetadataChildren(item.RatingKey)

			if err != nil {
				return newMediaItems, err
			}

			mediaItems, err := c.analyzeItems(content.MediaContainer)

			if err != nil {
				return newMediaItems, err
			}

			newMediaItems = append(newMediaItems, mediaItems...)
		} else if item.Type == "movie" || item.Type == "episode" {
			mediaItems, err := c.analyzeItem(item, container)

			if err != nil {
				return newMediaItems, err
			}

			newMediaItems = append(newMediaItems, mediaItems...)
		} else {
			return newMediaItems, errors.New(fmt.Sprintf("Unknown item type: %s", item.Type))
		}
	}

	return newMediaItems, nil
}

func (c *Collector) analyzeItem(item plex.Metadata, container plex.MediaContainer) ([]*MediaItem, error) {
	mediaItems := make([]*MediaItem, 0)

	for _, media := range item.Media {
		if media.DeletedAt != 0 {
			continue
		}

		if media.AudioChannels == 0 && media.VideoResolution == "" {
			continue
		}

		size := 0

		for _, part := range media.Part {
			size += part.Size
		}

		mediaItem := &MediaItem{
			id:                   media.ID,
			audioChannels:        media.AudioChannels,
			audioCodec:           media.AudioCodec,
			grandParentRatingKey: item.GrandparentRatingKey,
			mediaType:            item.Type,
			parentRatingKey:      item.ParentRatingKey,
			sectionKey:           strconv.Itoa(container.LibrarySectionID),
			size:                 size,
			videoCodec:           media.VideoCodec,
			videoResolution:      media.VideoResolution,
		}

		mediaItems = append(mediaItems, mediaItem)
	}

	return mediaItems, nil
}

var mediaCollection = make(map[int]*MediaItem, 0)
var labels = []string{"audio_channels", "audio_codec", "media_type", "video_codec", "video_resolution"}
var mediaItemsCount = promauto.NewGaugeVec(prometheus.GaugeOpts{
	Name: "plex_media_items_count_total",
	Help: "The total count of media items.",
}, labels)
var mediaItemsBytes = promauto.NewGaugeVec(prometheus.GaugeOpts{
	Name: "plex_media_items_bytes_total",
	Help: "The total bytes size of media items.",
}, labels)

func bootstrap(c *cli.Context) error {
	plexClient, err := plex.New(c.String("url"), c.String("token"))

	if err != nil {
		return err
	}

	_, err = plexClient.Test()

	if err != nil {
		return err
	}

	collector := &Collector{
		client: plexClient,
	}

	err = collector.Collect()

	if err != nil {
		return err
	}

	ticker := time.NewTicker(10 * time.Minute)
	quit := make(chan struct{})

	go func() {
		for {
			select {
			case <-ticker.C:
				err = collector.Collect()

				if err != nil {
					log.Println(err)
				}
			case <-quit:
				ticker.Stop()
				return
			}
		}
	}()

	http.Handle("/metrics", promhttp.Handler())
	http.ListenAndServe(fmt.Sprintf(":%d", c.Int("port")), nil)

	return nil
}

func main() {
	app := &cli.App{
		Name:  "plex-collector",
		Usage: "Stuff and things.",
		Flags: []cli.Flag{
			&cli.IntFlag{
				Name:    "port",
				Aliases: []string{"p"},
				Value:   9090,
				Usage:   "HTTP port to listen to.",
				EnvVars: []string{"HTTP_PORT"},
			},
			&cli.StringFlag{
				Name:     "token",
				Aliases:  []string{"t"},
				Usage:    "Authentication token for Plex Media Server.",
				EnvVars:  []string{"PLEX_TOKEN"},
				Required: true,
			},
			&cli.StringFlag{
				Name:     "url",
				Aliases:  []string{"u"},
				Usage:    "Base URL to Plex Media Server.",
				EnvVars:  []string{"PLEX_URL"},
				Required: true,
			},
		},
		Action: bootstrap,
	}

	err := app.Run(os.Args)

	if err != nil {
		log.Fatal(err)
	}
}
