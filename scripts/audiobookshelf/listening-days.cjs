// Preload into Audiobookshelf alongside its existing NODE_OPTIONS. The new
// route inherits ABS authentication and the standard per-user access check.
const Module = require('node:module')

const load = Module._load
Module._load = function (request, parent, isMain) {
  const ApiRouter = load.call(this, request, parent, isMain)
  if (request !== './routers/ApiRouter' || !parent?.filename.endsWith('/server/Server.js')) {
    return ApiRouter
  }

  const requireFromServer = Module.createRequire(parent.filename)
  const database = requireFromServer('./Database')
  const users = requireFromServer('./controllers/UserController')
  const init = ApiRouter.prototype.init
  ApiRouter.prototype.init = function () {
    this.router.get('/users/:id/listening-days', users.middleware.bind(this), async (req, res, next) => {
      const day = req.query.day
      if (day !== undefined && (typeof day !== 'string' || !/^\d{4}-\d{2}-\d{2}$/.test(day))) {
        return res.status(400).json({ error: 'Expected day in YYYY-MM-DD format' })
      }
      const dateFilter = day === undefined ? '' : 'AND date = :day'
      try {
        const rows = await database.sequelize.query(
          `SELECT date, SUM(CAST(timeListening AS REAL)) AS seconds
             FROM playbackSessions
            WHERE userId = :userId AND date IS NOT NULL AND date <> ''
              AND CAST(timeListening AS REAL) > 0 ${dateFilter}
            GROUP BY date`,
          { replacements: { userId: req.reqUser.id, day }, type: 'SELECT' }
        )
        const days = Object.fromEntries(rows.map(({ date, seconds }) => [date, seconds]))
        // A scoped request can distinguish an existing user with no playback
        // from an unavailable query or a user that does not exist.
        if (day !== undefined && days[day] === undefined) days[day] = 0
        return res.json({ days })
      } catch (error) {
        return next(error)
      }
    })
    return init.call(this)
  }
  return ApiRouter
}
