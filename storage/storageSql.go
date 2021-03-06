package storage

import (
	"bytes"
	"database/sql"
	"fmt"
	"html/template"
	"time"

	"github.com/VagabondDataNinjas/gizlinebot/domain"
	"github.com/go-sql-driver/mysql"

	_ "github.com/go-sql-driver/mysql"
)

type Sql struct {
	Db *sql.DB
}

func NewSql(conDsn string) (s *Sql, err error) {
	db, err := sql.Open("mysql", conDsn)
	if err != nil {
		return s, err
	}

	return &Sql{
		Db: db,
	}, nil
}

func (s *Sql) Close() error {
	return s.Db.Close()
}

func (s *Sql) AddRawLineEvent(eventType, rawevent string) error {
	stmt, err := s.Db.Prepare("INSERT INTO linebot_raw_events(eventtype, rawevent, timestamp) VALUES(?, ?, ?)")
	if err != nil {
		return err
	}
	defer stmt.Close()
	_, err = stmt.Exec(eventType, rawevent, int32(time.Now().UTC().Unix()))
	if err != nil {
		return err
	}

	return nil
}

// AddUserProfile adds a user profile
// if the user already exists in the table this method does nothing
func (s *Sql) AddUserProfile(userID, displayName string) error {
	stmt, err := s.Db.Prepare("INSERT INTO user_profiles(userId, displayName, timestamp) VALUES(?, ?, ?)")
	if err != nil {
		return err
	}
	defer stmt.Close()
	_, err = stmt.Exec(userID, displayName, int32(time.Now().UTC().Unix()))

	if err != nil {
		if mysqlErr := err.(*mysql.MySQLError); mysqlErr.Number == 1062 {
			// ignore duplicate entry errors for profiles
			return nil
		}
		return err
	}

	return nil
}

func (s *Sql) MarkProfileBotSurveyInited(userId string) error {
	stmt, err := s.Db.Prepare("UPDATE user_profiles SET bot_survey_inited = 1 WHERE userId = ?")
	if err != nil {
		return err
	}
	defer stmt.Close()
	_, err = stmt.Exec(userId)

	if err != nil {
		return err
	}

	return nil
}

func (s *Sql) GetUsersWithoutAnswers(delaySecs int64) (userIds []string, err error) {
	var (
		userId string
	)

	tsCompare := time.Now().UTC().Unix() - delaySecs
	rows, err := s.Db.Query(`SELECT p.userId FROM user_profiles p
		LEFT JOIN answers a ON a.userId = p.userId
		WHERE a.userId IS NULL AND p.bot_survey_inited = 0 AND p.timestamp < ?`, tsCompare)
	if err != nil {
		return userIds, err
	}
	defer rows.Close()

	userIds = make([]string, 0)
	for rows.Next() {
		err := rows.Scan(&userId)
		if err != nil {
			return userIds, err
		}
		userIds = append(userIds, userId)
	}

	err = rows.Err()
	if err != nil {
		return userIds, err
	}

	return userIds, nil
}

func (s *Sql) GetUserProfile(userId string) (profile domain.UserProfile, err error) {
	var (
		displayName string
		timestamp   int
	)
	err = s.Db.QueryRow(`SELECT displayName, timestamp
		FROM user_profiles where userId = ?`, userId).Scan(&displayName, &timestamp)
	if err != nil {
		if err == sql.ErrNoRows {
			return profile, nil
		}
		return profile, err
	}

	return domain.UserProfile{
		UserId:      userId,
		DisplayName: displayName,
		Timestamp:   timestamp,
	}, nil
}

func (s *Sql) UserHasAnswers(userId string) (bool, error) {
	var hasAnswers int
	err := s.Db.QueryRow(`SELECT count(id) FROM answers
		WHERE userId = ?`, userId).Scan(&hasAnswers)
	if err != nil {
		return false, err
	}

	if hasAnswers > 0 {
		return true, nil
	}
	return false, nil
}

func (s *Sql) UserGetLastAnswer(uid string) (domain.Answer, error) {
	var id uint
	var userId string
	var questionId string
	var answer string
	var timestamp int64
	err := s.Db.QueryRow(`SELECT id, userId, questionId, answer, timestamp FROM answers
		WHERE userId = ? AND answer != ""
		ORDER BY timestamp DESC
		LIMIT 0,1
		`, uid).Scan(&id, &userId, &questionId, &answer, &timestamp)
	if err != nil {
		var emptyAnswer domain.Answer
		return emptyAnswer, err
	}

	return domain.Answer{
		Id:         id,
		UserId:     userId,
		QuestionId: questionId,
		Answer:     answer,
		Timestamp:  time.Unix(timestamp, 0),
	}, nil
}

func (s *Sql) GetQuestions() (qs *domain.Questions, err error) {
	var (
		id           string
		questionText string
		weight       int
		channel      string
	)
	rows, err := s.Db.Query(`SELECT id, question, weight, channel FROM questions ORDER BY weight ASC`)
	if err != nil {
		return qs, err
	}
	defer rows.Close()

	qs = domain.NewQuestions()
	for rows.Next() {
		err := rows.Scan(&id, &questionText, &weight, &channel)
		if err != nil {
			return qs, err
		}
		err = qs.Add(id, questionText, weight, channel)
		if err != nil {
			return qs, err
		}
	}

	err = rows.Err()
	if err != nil {
		return qs, err
	}

	return qs, nil
}

type WelcomeMsgTplVars struct {
	UserId   string
	Hostname string
}

func (s *Sql) GetWelcomeMsgs(tplVars *WelcomeMsgTplVars) (msgs []string, err error) {
	var (
		msgRaw string
	)
	rows, err := s.Db.Query(`SELECT msg FROM welcome_msgs WHERE channel IN ("line", "both") ORDER BY weight ASC`)
	if err != nil {
		return msgs, err
	}
	defer rows.Close()

	msgs = make([]string, 0)
	for rows.Next() {
		err := rows.Scan(&msgRaw)
		if err != nil {
			return msgs, err
		}
		msg, err := s.applyWelcomeTpl(msgRaw, tplVars)
		if err != nil {
			return msgs, err
		}
		msgs = append(msgs, msg)
	}

	err = rows.Err()
	if err != nil {
		return msgs, err
	}

	return msgs, nil
}

type UserAnswerData struct {
	// @TODO embed domain.Answer
	// domain.Answer
	Id         uint
	UserId     string
	QuestionId string
	Answer     string
	Channel    string
	Timestamp  int
}

type UserGpsAnswerData struct {
	Id        uint
	UserId    string
	Address   string
	Lat       float64
	Lon       float64
	Timestamp int
	Channel   string
}

func (s *Sql) GetGpsAnswerData() (answerGpsData []UserGpsAnswerData, err error) {
	rows, err := s.Db.Query(`SELECT p.id, p.userId, IFNULL(a.address, ""), IFNULL(a.lat, 0.0), IFNULL(a.lon, 0.0), IFNULL(a.channel, ""), IFNULL(a.timestamp, 0) FROM user_profiles p
		LEFT JOIN answers_gps a ON a.userId = p.userId
		ORDER BY a.timestamp ASC
		`)
	if err != nil {
		return answerGpsData, err
	}
	defer rows.Close()

	answerGpsData = make([]UserGpsAnswerData, 0)
	for rows.Next() {
		a := UserGpsAnswerData{}
		err := rows.Scan(&a.Id, &a.UserId, &a.Address, &a.Lat, &a.Lon, &a.Channel, &a.Timestamp)
		if err != nil {
			return answerGpsData, err
		}
		answerGpsData = append(answerGpsData, a)
	}

	err = rows.Err()
	if err != nil {
		return answerGpsData, err
	}

	return answerGpsData, nil

}
func (s *Sql) GetUserAnswerData() (answerData []UserAnswerData, err error) {
	var (
		userId     string
		questionId string
		answer     string
		channel    string
		answerTime int
	)
	rows, err := s.Db.Query(`SELECT p.userId, IFNULL(a.questionId, ""), IFNULL(a.answer, ""), IFNULL(a.channel, ""), IFNULL(a.timestamp, 0) as answerTime FROM user_profiles p
		LEFT JOIN answers a ON a.userId = p.userId
		ORDER BY a.timestamp ASC
		`)
	if err != nil {
		return answerData, err
	}
	defer rows.Close()

	answerData = make([]UserAnswerData, 0)
	for rows.Next() {
		err := rows.Scan(&userId, &questionId, &answer, &channel, &answerTime)
		if err != nil {
			return answerData, err
		}
		answerData = append(answerData, UserAnswerData{
			UserId:     userId,
			QuestionId: questionId,
			Answer:     answer,
			Channel:    channel,
			Timestamp:  answerTime,
		})
	}

	err = rows.Err()
	if err != nil {
		return answerData, err
	}

	return answerData, nil
}

func (s *Sql) applyWelcomeTpl(msg string, tplVars *WelcomeMsgTplVars) (string, error) {
	tmpl, err := template.New("welcomeMsg").Parse(msg)
	if err != nil {
		return "", err
	}
	buf := new(bytes.Buffer)
	err = tmpl.Execute(buf, tplVars)
	if err != nil {
		return "", err
	}
	return buf.String(), nil
}

func (s *Sql) UserAddAnswer(answer domain.Answer) error {
	stmt, err := s.Db.Prepare("INSERT INTO answers(userId, questionId, answer, channel, timestamp) VALUES(?, ?, ?, ?, ?)")
	if err != nil {
		return err
	}
	defer stmt.Close()
	_, err = stmt.Exec(answer.UserId, answer.QuestionId, answer.Answer, answer.Channel, int32(time.Now().UTC().Unix()))

	if err != nil {
		return err
	}

	return nil
}

func (s *Sql) WipeUser(userId string) error {
	for _, table := range []string{"user_profiles", "answers", "answers_gps"} {
		err := s.deleteFromTableUserId(table, userId)
		if err != nil {
			return err
		}
	}

	return nil
}

func (s *Sql) deleteFromTableUserId(table string, userId string) error {
	// @TODO find out how to use dynamic table name in prepared query
	q := fmt.Sprintf("DELETE FROM %s WHERE userId = ?", table)
	stmt, err := s.Db.Prepare(q)
	if err != nil {
		return err
	}
	defer stmt.Close()
	_, err = stmt.Exec(userId)
	if err != nil {
		return err
	}

	return nil
}

func (s *Sql) UserAddGpsAnswer(answer domain.AnswerGps) error {
	stmt, err := s.Db.Prepare("INSERT INTO answers_gps(userId, lat, lon, address, channel, timestamp) VALUES(?, ?, ?, ?, ?, ?)")
	if err != nil {
		return err
	}
	defer stmt.Close()
	_, err = stmt.Exec(answer.UserId, answer.Lat, answer.Lon, answer.Address, answer.Channel, int32(time.Now().UTC().Unix()))

	if err != nil {
		return err
	}

	return nil
}
