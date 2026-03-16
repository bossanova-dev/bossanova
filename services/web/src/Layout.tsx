import { NavLink, Outlet } from 'react-router'
import { useAuth0 } from '@auth0/auth0-react'

export default function Layout() {
  const { isAuthenticated, loginWithRedirect, logout, user } = useAuth0()

  return (
    <>
      <header style={header}>
        <nav style={nav}>
          <strong style={{ fontSize: 18, color: 'var(--text-h)' }}>Bossanova</strong>
          <NavLink to="/" style={link}>Sessions</NavLink>
          <NavLink to="/daemons" style={link}>Daemons</NavLink>
        </nav>
        <div>
          {isAuthenticated ? (
            <span style={{ display: 'flex', alignItems: 'center', gap: 12 }}>
              <span style={{ fontSize: 14 }}>{user?.email}</span>
              <button
                onClick={() => logout({ logoutParams: { returnTo: window.location.origin } })}
                style={btn}
              >
                Log out
              </button>
            </span>
          ) : (
            <button onClick={() => loginWithRedirect()} style={btn}>
              Log in
            </button>
          )}
        </div>
      </header>
      <main style={{ flex: 1, padding: '24px 0' }}>
        <Outlet />
      </main>
    </>
  )
}

const header: React.CSSProperties = {
  display: 'flex',
  alignItems: 'center',
  justifyContent: 'space-between',
  padding: '12px 24px',
  borderBottom: '1px solid var(--border)',
}

const nav: React.CSSProperties = {
  display: 'flex',
  alignItems: 'center',
  gap: 16,
}

const link: React.CSSProperties = {
  color: 'var(--text)',
  textDecoration: 'none',
  fontSize: 15,
}

const btn: React.CSSProperties = {
  background: 'var(--accent)',
  color: '#fff',
  border: 'none',
  borderRadius: 4,
  padding: '6px 14px',
  cursor: 'pointer',
  fontSize: 14,
}
