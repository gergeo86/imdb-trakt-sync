package client

import (
	"encoding/csv"
	"fmt"
	"github.com/PuerkitoBio/goquery"
	"github.com/cecobask/imdb-trakt-sync/pkg/entities"
	"go.uber.org/zap"
	"mime"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const (
	imdbCookieNameAtMain   = "at-main"
	imdbCookieNameUbidMain = "ubid-main"

	imdbHeaderKeyContentDisposition = "Content-Disposition"

	imdbPathBase          = "https://www.imdb.com"
	imdbPathListExport    = "/list/%s/export"
	imdbPathLists         = "/user/%s/lists"
	imdbPathProfile       = "/profile"
	imdbPathRatingsExport = "/user/%s/ratings/export"
	imdbPathWatchlist     = "/watchlist"
)

type ImdbClient struct {
	client *http.Client
	config ImdbConfig
	logger *zap.Logger
}

type ImdbConfig struct {
	CookieAtMain   string
	CookieUbidMain string
	UserId         string
	WatchlistId    string
}

func NewImdbClient(config ImdbConfig, logger *zap.Logger) (ImdbClientInterface, error) {
	jar, err := setupCookieJar(config)
	if err != nil {
		return nil, err
	}
	client := &ImdbClient{
		client: &http.Client{
			Jar: jar,
		},
		config: config,
		logger: logger,
	}
	if err = client.hydrate(); err != nil {
		return nil, fmt.Errorf("failure hydrating imdb client: %w", err)
	}
	return client, nil
}

func setupCookieJar(config ImdbConfig) (http.CookieJar, error) {
	imdbUrl, err := url.Parse(imdbPathBase)
	if err != nil {
		return nil, fmt.Errorf("failure parsing %s as url: %w", imdbPathBase, err)
	}
	jar, err := cookiejar.New(nil)
	if err != nil {
		return nil, fmt.Errorf("failure creating cookie jar: %w", err)
	}
	jar.SetCookies(imdbUrl, []*http.Cookie{
		{
			Name:  imdbCookieNameAtMain,
			Value: config.CookieAtMain,
		},
		{
			Name:  imdbCookieNameUbidMain,
			Value: config.CookieUbidMain,
		},
	})
	return jar, nil
}

func (c *ImdbClient) hydrate() error {
	if c.config.UserId == "" || c.config.UserId == "scrape" {
		if err := c.UserIdScrape(); err != nil {
			return fmt.Errorf("failure scraping imdb user id: %w", err)
		}
	}
	if err := c.WatchlistIdScrape(); err != nil {
		return fmt.Errorf("failure scraping imdb watchlist id: %w", err)
	}
	return nil
}

func (c *ImdbClient) doRequest(reqFields entities.RequestFields) (*http.Response, error) {
	request, err := http.NewRequest(reqFields.Method, reqFields.Url, reqFields.Body)
	if err != nil {
		return nil, fmt.Errorf("failure creating http request %s %s: %w", reqFields.Method, reqFields.Url, err)
	}
	response, err := c.client.Do(request)
	if err != nil {
		return nil, fmt.Errorf("failure sending http request %s %s: %w", reqFields.Method, reqFields.Url, err)
	}
	switch response.StatusCode {
	case http.StatusOK:
		break
	case http.StatusForbidden:
		return nil, &ApiError{
			clientName: clientNameImdb,
			httpMethod: request.Method,
			url:        request.URL.String(),
			StatusCode: response.StatusCode,
			details:    "imdb authorization failure - update the imdb cookie values",
		}
	case http.StatusNotFound:
		break // handled individually in various functions
	default:
		return nil, &ApiError{
			clientName: clientNameImdb,
			httpMethod: request.Method,
			url:        request.URL.String(),
			StatusCode: response.StatusCode,
			details:    fmt.Sprintf("unexpected status code %d", response.StatusCode),
		}
	}
	return response, nil
}

func (c *ImdbClient) ListGet(listId string) (*entities.ImdbList, error) {
	path := fmt.Sprintf(imdbPathListExport, listId)
	requestFields := entities.RequestFields{
		Method:   http.MethodGet,
		Endpoint: imdbPathBase,
		Path:     path,
		Url:      imdbPathBase + path,
		Body:     http.NoBody,
	}
	response, err := c.doRequest(requestFields)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusNotFound {
		return nil, &ApiError{
			clientName: clientNameImdb,
			httpMethod: response.Request.Method,
			url:        response.Request.URL.String(),
			StatusCode: response.StatusCode,
			details:    fmt.Sprintf("list with id %s could not be found", listId),
		}
	}
	return readImdbListResponse(response, listId)
}

func (c *ImdbClient) WatchlistGet() (*entities.ImdbList, error) {
	list, err := c.ListGet(c.config.WatchlistId)
	if err != nil {
		return nil, err
	}
	list.IsWatchlist = true
	return list, nil
}

func (c *ImdbClient) ListsGetAll() ([]entities.ImdbList, error) {
	path := fmt.Sprintf(imdbPathLists, c.config.UserId)
	requestFields := entities.RequestFields{
		Method:   http.MethodGet,
		Endpoint: imdbPathBase,
		Path:     path,
		Url:      imdbPathBase + path,
		Body:     http.NoBody,
	}
	response, err := c.doRequest(requestFields)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	doc, err := goquery.NewDocumentFromReader(response.Body)
	if err != nil {
		return nil, fmt.Errorf("failure creating goquery document from imdb response: %w", err)
	}
	var lists []entities.ImdbList
	doc.Find(".user-list").Each(func(i int, selection *goquery.Selection) {
		listId, ok := selection.Attr("id")
		if !ok {
			c.logger.Info("found no imdb lists")
			return
		}
		list, err := c.ListGet(listId)
		if err != nil {
			c.logger.Error("unexpected error while scraping imdb lists", zap.Error(err))
			return
		}
		list.TraktListSlug = buildTraktListName(list.ListName)
		lists = append(lists, *list)
	})
	return lists, nil
}

func (c *ImdbClient) UserIdScrape() error {
	requestFields := entities.RequestFields{
		Method:   http.MethodGet,
		Endpoint: imdbPathBase,
		Path:     imdbPathProfile,
		Url:      imdbPathBase + imdbPathProfile,
		Body:     http.NoBody,
	}
	response, err := c.doRequest(requestFields)
	if err != nil {
		return err
	}
	userId, err := scrapeSelectionAttribute(response.Body, clientNameImdb, ".user-profile.userId", "data-userid")
	if err != nil {
		return fmt.Errorf("imdb user id not found: %w", err)
	}
	c.config.UserId = *userId
	return nil
}

func (c *ImdbClient) WatchlistIdScrape() error {
	requestFields := entities.RequestFields{
		Method:   http.MethodGet,
		Endpoint: imdbPathBase,
		Path:     imdbPathWatchlist,
		Url:      imdbPathBase + imdbPathWatchlist,
		Body:     http.NoBody,
	}
	response, err := c.doRequest(requestFields)
	if err != nil {
		return err
	}
	watchlistId, err := scrapeSelectionAttribute(response.Body, clientNameImdb, "meta[property='pageId']", "content")
	if err != nil {
		return fmt.Errorf("imdb watchlist id not found: %w", err)
	}
	c.config.WatchlistId = *watchlistId
	return nil
}

func (c *ImdbClient) RatingsGet() ([]entities.ImdbItem, error) {
	path := fmt.Sprintf(imdbPathRatingsExport, c.config.UserId)
	requestFields := entities.RequestFields{
		Method:   http.MethodGet,
		Endpoint: imdbPathBase,
		Path:     path,
		Url:      imdbPathBase + path,
		Body:     http.NoBody,
	}
	response, err := c.doRequest(requestFields)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	return readImdbRatingsResponse(response)
}

func readImdbListResponse(res *http.Response, listId string) (*entities.ImdbList, error) {
	csvReader := csv.NewReader(res.Body)
	csvReader.LazyQuotes = true
	csvReader.FieldsPerRecord = -1
	csvData, err := csvReader.ReadAll()
	if err != nil {
		return nil, fmt.Errorf("failure reading from imdb response: %w", err)
	}
	var listItems []entities.ImdbItem
	for i, record := range csvData {
		if i > 0 { // omit header line
			listItems = append(listItems, entities.ImdbItem{
				Id:        record[1],
				TitleType: record[7],
			})
		}
	}
	contentDispositionHeader := res.Header.Get(imdbHeaderKeyContentDisposition)
	if contentDispositionHeader == "" {
		return nil, fmt.Errorf("failure reading header %s from imdb response", imdbHeaderKeyContentDisposition)
	}
	_, params, err := mime.ParseMediaType(contentDispositionHeader)
	if err != nil || len(params) == 0 {
		return nil, fmt.Errorf("failure parsing media type from imdb header %s: %w", imdbHeaderKeyContentDisposition, err)
	}
	listName := strings.Split(params["filename"], ".")[0]
	return &entities.ImdbList{
		ListName:      listName,
		ListId:        listId,
		ListItems:     listItems,
		TraktListSlug: buildTraktListName(listName),
	}, nil
}

func readImdbRatingsResponse(res *http.Response) ([]entities.ImdbItem, error) {
	csvReader := csv.NewReader(res.Body)
	csvReader.LazyQuotes = true
	csvReader.FieldsPerRecord = -1
	csvData, err := csvReader.ReadAll()
	if err != nil {
		return nil, fmt.Errorf("failure reading from imdb response: %w", err)
	}
	var ratings []entities.ImdbItem
	for i, record := range csvData {
		if i > 0 {
			rating, err := strconv.Atoi(record[1])
			if err != nil {
				return nil, fmt.Errorf("failure parsing imdb rating value to integer: %w", err)
			}
			ratingDate, err := time.Parse("2006-01-02", record[2])
			if err != nil {
				return nil, fmt.Errorf("failure parsing imdb rating date: %w", err)
			}
			ratings = append(ratings, entities.ImdbItem{
				Id:         record[0],
				TitleType:  record[5],
				Rating:     &rating,
				RatingDate: &ratingDate,
			})
		}
	}
	return ratings, nil
}

func buildTraktListName(imdbListName string) string {
	formatted := strings.ToLower(strings.Join(strings.Fields(imdbListName), "-"))
	re := regexp.MustCompile(`[^-a-z0-9]+`)
	return re.ReplaceAllString(formatted, "")
}
