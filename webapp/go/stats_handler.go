package main

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"sort"
	"strconv"

	"github.com/labstack/echo/v4"
	"golang.org/x/sync/singleflight"
)

type LivestreamStatistics struct {
	Rank           int64 `json:"rank"`
	ViewersCount   int64 `json:"viewers_count"`
	TotalReactions int64 `json:"total_reactions"`
	TotalReports   int64 `json:"total_reports"`
	MaxTip         int64 `json:"max_tip"`
}

type LivestreamRankingEntry struct {
	LivestreamID int64
	Score        int64
}
type LivestreamRanking []LivestreamRankingEntry

func (r LivestreamRanking) Len() int      { return len(r) }
func (r LivestreamRanking) Swap(i, j int) { r[i], r[j] = r[j], r[i] }
func (r LivestreamRanking) Less(i, j int) bool {
	if r[i].Score == r[j].Score {
		return r[i].LivestreamID < r[j].LivestreamID
	} else {
		return r[i].Score < r[j].Score
	}
}

type UserStatistics struct {
	Rank              int64  `json:"rank"`
	ViewersCount      int64  `json:"viewers_count"`
	TotalReactions    int64  `json:"total_reactions"`
	TotalLivecomments int64  `json:"total_livecomments"`
	TotalTip          int64  `json:"total_tip"`
	FavoriteEmoji     string `json:"favorite_emoji"`
}

type UserRankingEntry struct {
	Username string
	Score    int64
}
type UserRanking []UserRankingEntry

func (r UserRanking) Len() int      { return len(r) }
func (r UserRanking) Swap(i, j int) { r[i], r[j] = r[j], r[i] }
func (r UserRanking) Less(i, j int) bool {
	if r[i].Score == r[j].Score {
		return r[i].Username < r[j].Username
	} else {
		return r[i].Score < r[j].Score
	}
}

var userRankingSingleflight singleflight.Group

func getUserRanking() (UserRanking, error) {
	resultI, err, _ := userRankingSingleflight.Do("user_ranking", func() (interface{}, error) {
		tx, err := dbConn.BeginTxx(context.Background(), nil)
		if err != nil {
			return nil, err
		}
		defer tx.Rollback()

		var users []*UserModel
		if err := tx.SelectContext(context.Background(), &users, "SELECT id, name FROM users"); err != nil {
			return nil, err
		}

		//var ranking UserRanking
		userRankingData := map[int64]UserRankingEntry{}
		for _, user := range users {
			userRankingData[user.ID] = UserRankingEntry{Username: user.Name}
		}

		// リアクション数
		type UserReaction struct {
			UserID    int64 `db:"user_id"`
			Reactions int64 `db:"reactions"`
		}
		var reactions []*UserReaction
		if err := tx.SelectContext(context.Background(), &reactions, `
		SELECT
			u.id AS user_id
			, COUNT(*) AS reactions
		FROM users u
		INNER JOIN livestreams l ON l.user_id = u.id
		INNER JOIN reactions r ON r.livestream_id = l.id
		GROUP BY u.id
		`); err != nil && !errors.Is(err, sql.ErrNoRows) {
			return nil, err
		}

		// チップ合計
		type UserTotalTip struct {
			UserID    int64 `db:"user_id"`
			TotalTips int64 `db:"total_tips"`
		}
		var totalTips []*UserTotalTip
		if err := tx.SelectContext(context.Background(), &totalTips, `
		SELECT
			u2.id AS user_id,
			IFNULL(SUM(lc.tip), 0) AS total_tips
		FROM users u2
		INNER JOIN livestreams ls ON ls.user_id = u2.id
		INNER JOIN livecomments lc ON lc.livestream_id = ls.id
		GROUP BY u2.id
		`); err != nil && !errors.Is(err, sql.ErrNoRows) {
			return nil, err
		}

		for _, reaction := range reactions {
			d := userRankingData[reaction.UserID]
			d.Score += reaction.Reactions
			userRankingData[reaction.UserID] = d
		}

		for _, tip := range totalTips {
			d := userRankingData[tip.UserID]
			d.Score += tip.TotalTips
			userRankingData[tip.UserID] = d
		}

		var ranking = make(UserRanking, 0, len(userRankingData))
		for _, entry := range userRankingData {
			ranking = append(ranking, entry)
		}

		sort.Sort(ranking)

		return ranking, nil
	})
	if err != nil {
		return UserRanking{}, err
	}

	return resultI.(UserRanking), nil
}

// ユーザの統計情報を取得する
func getUserStatisticsHandler(c echo.Context) error {
	ctx := c.Request().Context()

	if err := verifyUserSession(c); err != nil {
		// echo.NewHTTPErrorが返っているのでそのまま出力
		return err
	}

	username := c.Param("username")
	// ユーザごとに、紐づく配信について、累計リアクション数、累計ライブコメント数、累計売上金額を算出
	// また、現在の合計視聴者数もだす

	tx, err := dbConn.BeginTxx(ctx, nil)
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, "failed to begin transaction: "+err.Error())
	}
	defer tx.Rollback()

	var user UserModel
	if err := tx.GetContext(ctx, &user, "SELECT * FROM users WHERE name = ?", username); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return echo.NewHTTPError(http.StatusBadRequest, "not found user that has the given username")
		} else {
			return echo.NewHTTPError(http.StatusInternalServerError, "failed to get user: "+err.Error())
		}
	}

	// ランク算出
	ranking, err := getUserRanking()
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, "failed to get user ranking: "+err.Error())
	}

	var rank int64 = 1
	for i := len(ranking) - 1; i >= 0; i-- {
		entry := ranking[i]
		if entry.Username == username {
			break
		}
		rank++
	}

	// リアクション数
	var totalReactions int64
	query := `SELECT COUNT(*) FROM users u 
    INNER JOIN livestreams l ON l.user_id = u.id 
    INNER JOIN reactions r ON r.livestream_id = l.id
    WHERE u.name = ?
	`
	if err := tx.GetContext(ctx, &totalReactions, query, username); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return echo.NewHTTPError(http.StatusInternalServerError, "failed to count total reactions: "+err.Error())
	}

	// ライブコメント数、チップ合計
	var livecomments struct {
		Tips     int64 `db:"tips"`
		Comments int64 `db:"comments"`
	}
	if err := tx.GetContext(ctx, &livecomments, `
	  SELECT
	    SUM(tip) AS tips,
	    COUNT(*) AS comments
	  FROM 
	    livecomments
	  WHERE 
	    livestream_id IN (
	  	  SELECT
	  	    id
	  	  FROM
	  	    livestreams
	  	  WHERE
	  	    user_id = ?
	      )
	`, user.ID); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return echo.NewHTTPError(http.StatusInternalServerError, "failed to get livecomments: "+err.Error())
	}

	// 合計視聴者数
	var viewersCount int64
	if err := tx.GetContext(ctx, &viewersCount, `
	  SELECT COUNT(*) FROM livestream_viewers_history WHERE livestream_id IN (SELECT id FROM livestreams WHERE user_id = ?)
	`, user.ID); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return echo.NewHTTPError(http.StatusInternalServerError, "failed to count viewers: "+err.Error())
	}

	// お気に入り絵文字
	var favoriteEmoji string
	query = `
	SELECT r.emoji_name
	FROM users u
	INNER JOIN livestreams l ON l.user_id = u.id
	INNER JOIN reactions r ON r.livestream_id = l.id
	WHERE u.name = ?
	GROUP BY emoji_name
	ORDER BY COUNT(*) DESC, emoji_name DESC
	LIMIT 1
	`
	if err := tx.GetContext(ctx, &favoriteEmoji, query, username); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return echo.NewHTTPError(http.StatusInternalServerError, "failed to find favorite emoji: "+err.Error())
	}

	stats := UserStatistics{
		Rank:              rank,
		ViewersCount:      viewersCount,
		TotalReactions:    totalReactions,
		TotalLivecomments: livecomments.Comments,
		TotalTip:          livecomments.Tips,
		FavoriteEmoji:     favoriteEmoji,
	}
	return c.JSON(http.StatusOK, stats)
}

// ライブストリームの統計情報を取得する
func getLivestreamStatisticsHandler(c echo.Context) error {
	ctx := c.Request().Context()

	if err := verifyUserSession(c); err != nil {
		return err
	}

	id, err := strconv.Atoi(c.Param("livestream_id"))
	if err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "livestream_id in path must be integer")
	}
	livestreamID := int64(id)

	tx, err := dbConn.BeginTxx(ctx, nil)
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, "failed to begin transaction: "+err.Error())
	}
	defer tx.Rollback()

	var livestream LivestreamModel
	if err := tx.GetContext(ctx, &livestream, "SELECT * FROM livestreams WHERE id = ?", livestreamID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return echo.NewHTTPError(http.StatusBadRequest, "cannot get stats of not found livestream")
		} else {
			return echo.NewHTTPError(http.StatusInternalServerError, "failed to get livestream: "+err.Error())
		}
	}

	var livestreams []struct {
		ID int64 `db:"id"`
	}
	if err := tx.SelectContext(ctx, &livestreams, "SELECT id FROM livestreams"); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return echo.NewHTTPError(http.StatusInternalServerError, "failed to get livestreams: "+err.Error())
	}
	data := map[int64]LivestreamRankingEntry{}
	for _, s := range livestreams {
		data[s.ID] = LivestreamRankingEntry{LivestreamID: s.ID}
	}

	// ライブごとのリアクション数
	type LiveReaction struct {
		LivestreamID int64 `db:"livestream_id"`
		Reactions    int64 `db:"reactions"`
	}
	var reactions []*LiveReaction

	if err := tx.SelectContext(ctx, &reactions, `
		SELECT
			r.livestream_id AS livestream_id
			, COUNT(*) AS reactions
		FROM reactions r
		GROUP BY r.livestream_id
	`); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return echo.NewHTTPError(http.StatusInternalServerError, "failed to count reactions: "+err.Error())
	}

	// ライブごとのチップ合計取得
	type LiveTotalTip struct {
		ID        int64 `db:"id"`
		TotalTips int64 `db:"total_tips"`
	}
	var totalTips []*LiveTotalTip

	if err := tx.SelectContext(ctx, &totalTips, `
	SELECT
		l2.livestream_id AS id,
		IFNULL(SUM(l2.tip), 0) AS total_tips
	FROM livecomments l2
	GROUP BY l2.livestream_id
	`); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return echo.NewHTTPError(http.StatusInternalServerError, "failed to get livestreams: "+err.Error())
	}

	// リアクション数
	for _, reaction := range reactions {
		d := data[reaction.LivestreamID]
		d.Score += reaction.Reactions
		data[reaction.LivestreamID] = d
	}
	totalReactions := data[livestream.ID].Score

	// ランク算出
	for _, tip := range totalTips {
		d := data[tip.ID]
		d.Score += tip.TotalTips
		data[tip.ID] = d
	}
	var ranking = make(LivestreamRanking, 0, len(data))
	for _, entry := range data {
		ranking = append(ranking, entry)
	}
	sort.Sort(ranking)

	var rank int64 = 1
	for i := len(ranking) - 1; i >= 0; i-- {
		entry := ranking[i]
		if entry.LivestreamID == livestreamID {
			break
		}
		rank++
	}

	// 視聴者数算出
	var viewersCount int64
	if err := tx.GetContext(ctx, &viewersCount, `SELECT COUNT(*) FROM livestreams l INNER JOIN livestream_viewers_history h ON h.livestream_id = l.id WHERE l.id = ?`, livestreamID); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return echo.NewHTTPError(http.StatusInternalServerError, "failed to count livestream viewers: "+err.Error())
	}

	// 最大チップ額
	var maxTip int64
	if err := tx.GetContext(ctx, &maxTip, `SELECT IFNULL(MAX(tip), 0) FROM livestreams l INNER JOIN livecomments l2 ON l2.livestream_id = l.id WHERE l.id = ?`, livestreamID); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return echo.NewHTTPError(http.StatusInternalServerError, "failed to find maximum tip livecomment: "+err.Error())
	}

	// リアクション数
	//var totalReactions int64
	//if err := tx.GetContext(ctx, &totalReactions, "SELECT COUNT(*) FROM livestreams l INNER JOIN reactions r ON r.livestream_id = l.id WHERE l.id = ?", livestreamID); err != nil && !errors.Is(err, sql.ErrNoRows) {
	//	return echo.NewHTTPError(http.StatusInternalServerError, "failed to count total reactions: "+err.Error())
	//}

	// スパム報告数
	var totalReports int64
	if err := tx.GetContext(ctx, &totalReports, `SELECT COUNT(*) FROM livestreams l INNER JOIN livecomment_reports r ON r.livestream_id = l.id WHERE l.id = ?`, livestreamID); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return echo.NewHTTPError(http.StatusInternalServerError, "failed to count total spam reports: "+err.Error())
	}

	if err := tx.Commit(); err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, "failed to commit: "+err.Error())
	}

	return c.JSON(http.StatusOK, LivestreamStatistics{
		Rank:           rank,
		ViewersCount:   viewersCount,
		MaxTip:         maxTip,
		TotalReactions: totalReactions,
		TotalReports:   totalReports,
	})
}
