import forge from 'node-forge';
import moment from 'moment';
import KJUR from 'jsrsasign';
import { v4 as uuid } from 'uuid';
import Cookies from 'js-cookie';

function Account(api) {
  const inSixHours = new Date(new Date().getTime() + 6 * 60 * 60 * 1000);
  Cookies.defaults.expires = inSixHours;
  this.api = api;
}

Account.prototype = {
  newVerification: function (callback, params) {
    this.api.request('POST', '/verifications', params, function(resp) {
      return callback(resp);
    });
  },

  verifyVerification: function (callback, params) {
    this.api.request('POST', '/verifications/' + params['verification_id'], params, function(resp) {
      return callback(resp);
    });
  },

  createUser: function (callback, params) {
    const self = this;
    var pwd = uuid().toLowerCase();
    var ec = new KJUR.crypto.ECDSA({'curve': 'secp256r1'});
    var pub = ec.generateKeyPairHex().ecpubhex;
    var priv = KJUR.KEYUTIL.getPEM(ec, 'PKCS8PRV', pwd);

    params['session_secret'] = '3059301306072a8648ce3d020106082a8648ce3d030107034200' + pub;
    this.api.request('POST', '/users', params, function(resp) {
      if (resp.data) {
        self.clear();
        Cookies.set('sid', pwd);
        self.store(priv, resp.data);
      }
      return callback(resp);
    });
  },

  resetPassword: function (callback, params) {
    const self = this;
    var pwd = uuid().toLowerCase();
    var ec = new KJUR.crypto.ECDSA({'curve': 'secp256r1'});
    var pub = ec.generateKeyPairHex().ecpubhex;
    var priv = KJUR.KEYUTIL.getPEM(ec, 'PKCS8PRV', pwd);

    params['session_secret'] = '3059301306072a8648ce3d020106082a8648ce3d030107034200' + pub;
    this.api.request('POST', '/passwords', params, function(resp) {
      if (resp.data) {
        self.clear();
        Cookies.set('sid', pwd);
        self.store(priv, resp.data);
      }
      return callback(resp);
    });
  },

  createSession: function (callback, params) {
    const self = this;
    var pwd = uuid().toLowerCase();
    var ec = new KJUR.crypto.ECDSA({'curve': 'secp256r1'});
    var pub = ec.generateKeyPairHex().ecpubhex;
    var priv = KJUR.KEYUTIL.getPEM(ec, 'PKCS8PRV', pwd);

    params['session_secret'] = '3059301306072a8648ce3d020106082a8648ce3d030107034200' + pub;
    this.api.request('POST', '/sessions', params, function(resp) {
      if (resp.data) {
        self.clear();
        Cookies.set('sid', pwd);
        self.store(priv, resp.data);
      }
      if (resp.error) {
        self.api.notify('error', window.i18n.t('general.errors.'+resp.error.code));
      }
      return callback(resp);
    });
  },

  verifySession: function (callback, params) {
    const self = this;
    self.api.request('POST', '/sessions/'+params.session_id, params, function (resp) {
      if (typeof callback === 'function') {
        return callback(resp);
      }
    });
  },

  me: function (callback) {
    this.api.request('GET', '/me', undefined, function(resp) {
      if (typeof callback === 'function') {
        return callback(resp);
      }
    });
  },

  connectMixin: function (callback, code) {
    this.api.request('POST', '/me/mixin', {code: code}, function(resp) {
      return callback(resp);
    });
  },

  ecdsa: function () {
    var priv = window.localStorage.getItem('token.example');
    var pwd = Cookies.get('sid');
    if (!priv || !pwd) {
      return "";
    }
    var ec = KJUR.KEYUTIL.getKey(priv, pwd);
    return KJUR.KEYUTIL.getPEM(ec, 'PKCS1PRV');
  },

  token: function (method, uri, body) {
    var priv = window.localStorage.getItem('token.example');
    var pwd = Cookies.get('sid');
    if (!priv || !pwd) {
      return "";
    }
    Cookies.set('sid', pwd);

    var uid = window.localStorage.getItem('uid');
    var sid = window.localStorage.getItem('sid');
    return this.sign(uid, sid, priv, method, uri, body);
  },

  sign: function (uid, sid, privateKey, method, uri, body) {
    if (typeof body !== 'string') { body = ""; }

    let expire = moment.utc().add(1, 'minutes').unix();
    let md = forge.md.sha256.create();
    md.update(method + uri + body);

    var oHeader = {alg: 'ES256', typ: 'JWT'};
    var oPayload = {
      uid: uid,
      sid: sid,
      exp: expire,
      jti: uuid(),
      sig: md.digest().toHex()
    };
    var sHeader = JSON.stringify(oHeader);
    var sPayload = JSON.stringify(oPayload);
    var pwd = Cookies.get('sid');
    try {
      var k = KJUR.KEYUTIL.getKey(privateKey, pwd);
    } catch (e) {
      return '';
    }
    return KJUR.jws.JWS.sign('ES256', sHeader, sPayload, privateKey, pwd);
  },

  oceanToken: function (callback) {
    this.externalToken("OCEAN", "", callback);
  },

  mixinToken: function (uri, callback) {
    this.externalToken("MIXIN", uri, callback);
  },

  externalToken: function (category, uri, callback) {
    var key = 'token.' + category.toLowerCase() + uri;
    var token = window.localStorage.getItem(key);
    if (token) {
      return callback({data: {token: token}});
    }
    var params = {
      category: category,
      uri: uri
    };
    this.api.request('POST', '/tokens', params, function(resp) {
      if (resp.data) {
        window.localStorage.setItem(key, resp.data.token);
      }
      return callback(resp);
    });
  },

  clear: function () {
    var d = window.localStorage.getItem('market.default');
    window.localStorage.clear();
    if (d) {
      window.localStorage.setItem('market.default', d);
    }
  },

  store: function (priv, user) {
    window.localStorage.setItem('token.example', priv);
    window.localStorage.setItem('uid', user.user_id);
    window.localStorage.setItem('sid', user.session_id);
    if (user.mixin_id != null && user.mixin_id != undefined) {
      window.localStorage.setItem('mixin.id', user.mixin_id);
    }
  },

  isNotMixinUser: function () {
    let mixinId = window.localStorage.getItem('mixin.id');
    return mixinId == null || mixinId == undefined;
  }
}

export default Account;
