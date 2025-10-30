import pytest
import requests
import uuid
import os

BASE_URL = os.environ.get("API_BASE_URL", "http://localhost:8080")


@pytest.fixture
def unique_user_payload():
    """
    Pytest fixture to generate a unique user payload for registration.
    """
    unique_id = uuid.uuid4()
    return {
        "username": f"testuser_{unique_id}",
        "password": f"password_{uuid.uuid4()}",
        "email": f"test_{unique_id}@example.com",
    }


def test_health_check():
    """
    Tests if the /health endpoint is working.
    """
    response = requests.get(f"{BASE_URL}/health")
    assert response.status_code == 200
    assert response.text == "OK"


def test_user_registration_success(unique_user_payload):
    """
    Tests successful user registration.
    """
    response = requests.post(f"{BASE_URL}/register", json=unique_user_payload)
    assert response.status_code == 201
    response_json = response.json()
    assert response_json["status"] == "success"
    assert response_json["message"] == "User registered successfully"


def test_user_registration_failure_duplicate_user(unique_user_payload):
    """
    Tests that registration fails if the user already exists.
    """
    # First registration should succeed
    response1 = requests.post(f"{BASE_URL}/register", json=unique_user_payload)
    assert response1.status_code == 201

    # Second registration with the same data should fail
    response2 = requests.post(f"{BASE_URL}/register", json=unique_user_payload)
    assert response2.status_code == 409  # Conflict
    assert "Username or email already exists" in response2.text
